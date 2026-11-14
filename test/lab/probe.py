"""Read probes for the sealed Compose lab. Only projected evidence is exported."""

import configparser
import json
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from datetime import datetime, timezone
from pathlib import Path

TARGETS = {
    "sonarr": (8989, "4.0.20.3014", "includeUnknownSeriesItems"),
    "radarr": (7878, "6.4.4.10685", "includeUnknownMovieItems"),
}
SAB_VERSION = "5.1.3"


class ProbeFailure(RuntimeError):
    def __init__(self, phase, category, http_status=None):
        self.details = {"phase": phase, "category": category}
        if http_status is not None:
            self.details["httpStatus"] = http_status
        super().__init__(phase, category)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())


def get_json(origin, path, query="", headers=None):
    request = urllib.request.Request(
        origin + path + ("?" + urllib.parse.urlencode(query) if query else ""),
        headers=headers or {},
    )
    with HTTP.open(request, timeout=5) as response:
        contents = response.read(2_000_001)
        if len(contents) > 2_000_000:
            raise ValueError("Lab response exceeded the JSON budget")
        return json.loads(contents)


def wait_for(observe, timeout=120):
    deadline = time.monotonic() + timeout
    while True:
        try:
            return observe()
        except (ProbeFailure, OSError, ValueError, ET.ParseError, KeyError, TypeError) as error:
            if time.monotonic() >= deadline:
                if isinstance(error, ProbeFailure):
                    raise error from None
                raise ProbeFailure("read", "invalidResponseOrUnavailable") from None
            time.sleep(2)


def arr_snapshot(product, port, version, unknown_flag):
    origin = f"http://{product}:{port}"
    config = ET.parse(Path("/config", product, "config.xml"))
    api_key = config.getroot().findtext("ApiKey")
    if not api_key:
        raise ValueError("Lab API key is not available yet")
    headers = {"X-Api-Key": api_key}
    status = get_json(origin, "/api/v3/system/status", headers=headers)
    if status.get("appName", "").lower() != product or status.get("version") != version:
        raise ValueError("Unexpected installed Arr product or version")
    health = get_json(origin, "/api/v3/health", headers=headers)
    queue = get_json(
        origin,
        "/api/v3/queue",
        {"page": 1, "pageSize": 1, unknown_flag: "true"},
        headers,
    )
    if not isinstance(health, list) or queue["records"] != [] or queue["totalRecords"] != 0:
        raise ValueError("The disposable Arr service did not report an empty inventory")
    return {
        "product": product,
        "version": status["version"],
        "branch": status.get("branch"),
        "api": "v3",
        "healthFindings": len(health),
        "queueTotal": queue["totalRecords"],
    }


def sab_key(path=Path("/config/sab/sabnzbd.ini")):
    # SAB's ConfigObj format places __version__ and __encoding__ before [misc].
    # Give the preamble its own section so Python 3.12 can read the misc values.
    parser = configparser.ConfigParser(interpolation=None, strict=False)
    try:
        with path.open(encoding="utf-8-sig") as contents:
            text = contents.read(1_000_001)
        if len(text) > 1_000_000:
            raise ProbeFailure("config", "oversizedConfig")
        parser.read_string("[preamble]\n" + text)
    except OSError:
        raise ProbeFailure("config", "configUnavailable") from None
    except (configparser.Error, UnicodeError):
        raise ProbeFailure("config", "invalidConfig") from None
    api_key = parser.get("misc", "api_key", fallback="")
    if len(api_key) >= 2 and api_key[0] == api_key[-1] and api_key[0] in "\"'":
        api_key = api_key[1:-1]
    if not re.fullmatch(r"[0-9a-f]{32}", api_key):
        raise ProbeFailure("config", "missingOrInvalidKey")
    return api_key


def sab_read(phase, query):
    try:
        return get_json("http://sab:8080", "/api", query)
    except urllib.error.HTTPError as error:
        raise ProbeFailure(phase, "httpError", error.code) from None
    except OSError:
        raise ProbeFailure(phase, "unavailable") from None
    except ValueError:
        raise ProbeFailure(phase, "invalidResponse") from None


def sab_snapshot():
    version = sab_read("version", {"mode": "version", "output": "json"})
    if not isinstance(version, dict) or version.get("version") != SAB_VERSION:
        raise ProbeFailure("version", "unexpectedVersion")
    queue = sab_read("queue", {"mode": "queue", "output": "json", "apikey": sab_key()})
    if not isinstance(queue, dict) or not isinstance(queue.get("queue"), dict):
        raise ProbeFailure("queue", "invalidResponse")
    if queue["queue"].get("slots") != []:
        raise ProbeFailure("queue", "unexpectedSlots")
    return {"product": "sabnzbd", "version": version["version"], "queueTotal": 0}


def main():
    source_sha, images = sys.argv[1:]
    report = {
        "sourceSHA": source_sha,
        "measuredAt": datetime.now(timezone.utc).isoformat(),
        "images": images.splitlines(),
        "evidenceClass": "observedRead",
        "results": {},
    }
    observers = {
        product: lambda product=product, args=args: arr_snapshot(product, *args)
        for product, args in TARGETS.items()
    }
    observers["sabnzbd"] = sab_snapshot
    failed = False
    for product, observe in observers.items():
        started = time.monotonic()
        try:
            report["results"][product] = {"status": "verified", **wait_for(observe)}
        except ProbeFailure as error:
            report["results"][product] = {"status": "failed", "reason": "probeDeadline", **error.details}
            print(f"{product} probe failed: {json.dumps(error.details)}", file=sys.stderr)
            failed = True
        report["results"][product]["elapsedSeconds"] = round(time.monotonic() - started, 3)
    print(json.dumps(report, indent=2))
    return int(failed)


if __name__ == "__main__":
    sys.exit(main())
