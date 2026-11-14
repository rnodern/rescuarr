"""Read probes for the sealed Compose lab. Only projected evidence is exported."""

import configparser
import json
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
        except (OSError, ValueError, ET.ParseError, configparser.Error, KeyError, TypeError):
            if time.monotonic() >= deadline:
                raise RuntimeError("Lab probe did not produce a usable response before its deadline") from None
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


def sab_snapshot():
    origin = "http://sab:8080"
    version = get_json(origin, "/api", {"mode": "version", "output": "json"})
    if version.get("version") != SAB_VERSION:
        raise ValueError("Unexpected installed SABnzbd version")
    config = configparser.ConfigParser(interpolation=None, strict=False)
    config.read("/config/sab/sabnzbd.ini")
    api_key = config["misc"]["api_key"]
    if not api_key:
        raise ValueError("Lab API key is not available yet")
    queue = get_json(origin, "/api", {"mode": "queue", "output": "json", "apikey": api_key})
    if queue["queue"]["slots"] != []:
        raise ValueError("The disposable SABnzbd service did not report an empty inventory")
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
        except RuntimeError:
            report["results"][product] = {"status": "failed", "reason": "probeDeadline"}
            failed = True
        report["results"][product]["elapsedSeconds"] = round(time.monotonic() - started, 3)
    print(json.dumps(report, indent=2))
    return int(failed)


if __name__ == "__main__":
    sys.exit(main())
