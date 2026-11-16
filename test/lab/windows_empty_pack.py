"""Run recovery scenarios with native disposable Windows Sonarr and SABnzbd."""
import argparse
import hashlib
import json
import os
import platform
import secrets
import subprocess
import sys
import tempfile
from contextlib import ExitStack
from datetime import datetime, timezone
from functools import partial
from pathlib import Path
from unittest.mock import patch
from xml.etree import ElementTree as ET

import empty_pack
import language_pack
import scenarios
from empty_pack import empty_pack_replacement, run_go_adapter
from manual_browser import PackagedManualLab, require_ready, start_process as start, stop_process as stop
from manual_import_browser import run_packaged_id_only_copy
from probe import SAB_VERSION, SONARR_V5_VERSION, get_json
from source import SEED, serve

SONARR = "http://127.0.0.1:18989"
SAB = "http://127.1.1.1:18080"
SOURCE = "http://127.0.0.1:8081"
CASE = "sonarr/windows-v5-adapter-pack"
LANGUAGE_CASE = "sonarr/windows-v5-language-pack"
MANUAL_CASE = "sonarr/windows-v5-manual-pack"
MANUAL_COPY_CASE = "sonarr/windows-v5-manual-copy"
NORMAL_SETTINGS_COPY_CASE = "sonarr/windows-v5-copy-normal-settings"
NORMAL_SETTINGS_MANUAL_COPY_CASE = "sonarr/windows-v5-manual-copy-normal-settings"


def stage(result, name):
    result["stage"] = name
    print("Windows Sonarr lab: " + name, flush=True)


def sonarr_config(data, key):
    root = ET.Element("Config")
    for name, value in {
        "BindAddress": "127.0.0.1", "Port": "18989", "EnableSsl": "False",
        "LaunchBrowser": "False", "ApiKey": key, "AuthenticationEnabled": "True",
        "AuthenticationMethod": "Forms",
        "AuthenticationRequired": "Enabled", "Branch": "develop", "UpdateAutomatically": "False",
    }.items():
        ET.SubElement(root, name).text = value
    ET.ElementTree(root).write(data / "config.xml", encoding="utf-8")


def exercise(args, result, source_sha):
    scenarios.verify(os.name == "nt" and os.environ.get("GITHUB_ACTIONS") == "true", "requiresDisposableWindowsRunner")
    result["binaries"] = {}
    for name in ("sonarr", "sab", "adapter", "manual_copy", "normal_settings_manual_copy", "rescuarr"):
        executable = getattr(args, name)
        if executable is None:
            continue
        with executable.open("rb") as handle:
            result["binaries"][name] = {"file": executable.name, "sha256": hashlib.file_digest(handle, "sha256").hexdigest()}
    with ExitStack() as stack:
        root = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix="rescuarr-lab-"))).resolve()
        data, library, downloads = root / "sonarr", root / "library", root / "downloads"
        for path in (data, library, downloads / "complete", downloads / "incomplete"):
            path.mkdir(parents=True)
        sonarr_key, sab_key = (secrets.token_hex(16) for _ in range(2))
        sonarr_config(data, sonarr_key)
        ini = root / "sabnzbd.ini"
        ini.write_text(
            f"[misc]\nhost = 127.1.1.1\nport = 18080\napi_key = {sab_key}\n"
            f"download_dir = {downloads / 'incomplete'}\ncomplete_dir = {downloads / 'complete'}\n"
            "permissions = \napi_warnings = 0\n", encoding="utf-8",
        )
        for key, value in {"SAB": SAB, "SOURCE": SOURCE, "LIBRARY": library, "DOWNLOADS": downloads,
                           "sab_key": lambda: sab_key}.items():
            stack.enter_context(patch.object(scenarios, key, value))
        stack.enter_context(patch.object(empty_pack, "SOURCE", SOURCE))
        stack.enter_context(patch.object(language_pack, "SOURCE", SOURCE))

        stage(result, "sourceStartup")
        fixture = start(stack, [sys.executable, "-B", __file__, "--fixture",
                                args.media, args.certificate, args.key], root)
        health = require_ready(fixture, lambda: get_json(SOURCE, "/health"), "fixture")
        scenarios.verify(health.get("seed") == SEED, "unexpectedFixtureVersion")

        stage(result, "sabStartup")
        sab = start(stack, [args.sab, "-f", ini, "-s", "127.1.1.1:18080", "-b", "0"], args.sab.parent)
        version = require_ready(sab, lambda: scenarios.sab_api("version"), "sabStartup")
        scenarios.verify(version.get("version") == SAB_VERSION, "unexpectedSABVersion")
        scenarios.verify(scenarios.sab_api("queue")["queue"]["slots"] == []
                         and scenarios.sab_api("history")["history"]["slots"] == [], "sabNotEmpty")
        result["sabnzbd"] = {"version": SAB_VERSION, "loopback": True, "emptyAtStartup": True}
        scenarios.configure_sab()

        stage(result, "sonarrStartup")
        sonarr = start(stack, [args.sonarr, f"/data={data}", "/nobrowser"], args.sonarr.parent)
        arr = scenarios.Arr("sonarr", origin=SONARR, key=sonarr_key)
        status = require_ready(sonarr, lambda: get_json(SONARR, "/api/v3/system/status", headers=arr.headers),
                               "sonarrStartup")
        scenarios.verify(status.get("appName") == "Sonarr" and status.get("version") == SONARR_V5_VERSION
                         and status.get("branch") == "develop" and status.get("isWindows") is True
                         and status.get("isDocker") is False
                         and Path(status["appData"]).resolve() == data, "unexpectedSonarrInstallation")
        scenarios.verify(arr.read("series") == [] and arr.read("downloadclient") == []
                         and arr.read("rootfolder") == [], "sonarrNotEmpty")
        result["sonarr"] = {"version": SONARR_V5_VERSION, "branch": "develop", "windows": True,
                            "docker": False, "emptyAtStartup": True}

        stage(result, "arrSetup")
        sonarr_cleanup = bool(args.normal_settings_copy or args.normal_settings_manual_copy)
        completed_handling = bool(args.language_pack or args.manual_copy or sonarr_cleanup)
        profile = arr.setup(completed_handling=completed_handling, remove_completed=sonarr_cleanup,
                            remove_failed=sonarr_cleanup)
        result["settings"] = {"profileId": profile["id"], "cutoff": profile["cutoff"],
                              "completedHandling": completed_handling,
                              "removeCompletedDownloads": sonarr_cleanup, "removeFailedDownloads": sonarr_cleanup,
                              "automaticRedownloadFailed": True}
        if sonarr_cleanup:
            enable_normal_copy_settings(arr, result)
        stage(result, "mediaSetup")
        episodes = arr.media(profile["id"])
        stage(result, "exercise")
        if args.language_pack:
            language_pack.language_pack_replacement(arr, episodes[0], episodes[1], result)
        elif args.normal_settings_copy:
            scenarios.id_only_import(arr, episodes[0], episodes[1], result, observe_cleanup=True)
        elif args.manual_copy or args.normal_settings_manual_copy:
            run_packaged_id_only_copy(arr, episodes[0], episodes[1], result,
                                      args.manual_copy or args.normal_settings_manual_copy, source_sha,
                                      partial(stage, result), sonarr_cleanup=sonarr_cleanup)
        else:
            adapter = partial(run_go_adapter, executable=args.adapter) if args.adapter else None
            manual_review = None
            if args.rescuarr:
                manual = PackagedManualLab(args.rescuarr, root, stack, start, require_ready, stop,
                                           partial(stage, result), SOURCE, source_sha)
                manual_review = partial(manual.run, arr)
            empty_pack_replacement(arr, episodes[0], episodes[1], result, adapter=adapter, manual_review=manual_review)
        result["fixtureMetadata"] = get_json(SOURCE, "/lab/metadata")
        stage(result, "cleanup")
    result.update(status="observed", stage="complete", qualification="awaitingIndependentReview")


def enable_normal_copy_settings(arr, result):
    clients = arr.read("downloadclient")
    scenarios.verify(len(clients) == 1 and clients[0].get("implementation") == "Sabnzbd"
                     and clients[0].get("enable") is True
                     and clients[0].get("removeCompletedDownloads") is True
                     and clients[0].get("removeFailedDownloads") is True,
                     "normalCopyClientSettingsNotApplied")
    config = arr.read("config/mediamanagement")
    scenarios.verify(type(config.get("id")) is int and config["id"] > 0, "normalCopyMissingMediaManagement")
    route = f"config/mediamanagement/{config['id']}"
    arr.update(route, {**config, "copyUsingHardlinks": True})
    saved = arr.read(route)
    scenarios.verify(saved.get("copyUsingHardlinks") is True, "normalCopyHardlinksNotApplied")
    result["settings"]["copyUsingHardlinks"] = saved["copyUsingHardlinks"]


def main():
    if len(sys.argv) == 5 and sys.argv[1] == "--fixture":
        serve(Path(sys.argv[2]), sys.argv[3], sys.argv[4], bind="127.0.0.1", index_origin=SOURCE)
        return 0
    parser = argparse.ArgumentParser(description=__doc__)
    for arg in ("sonarr", "sab", "media", "certificate", "key", "report"):
        parser.add_argument(f"--{arg}", type=Path, required=True)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--adapter", type=Path)
    mode.add_argument("--language-pack", action="store_true")
    mode.add_argument("--rescuarr", type=Path)
    mode.add_argument("--manual-copy", type=Path)
    mode.add_argument("--normal-settings-copy", action="store_true")
    mode.add_argument("--normal-settings-manual-copy", type=Path)
    args = parser.parse_args()
    for attr, value in vars(args).items():
        if isinstance(value, Path):
            setattr(args, attr, value.resolve())

    result = {"status": "failed", "evidenceClass": "labObservation", "protocol": "usenet", "stage": "startup"}
    source_sha = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    scenario = (NORMAL_SETTINGS_MANUAL_COPY_CASE if args.normal_settings_manual_copy else
                NORMAL_SETTINGS_COPY_CASE if args.normal_settings_copy else LANGUAGE_CASE if args.language_pack else
                MANUAL_COPY_CASE if args.manual_copy else MANUAL_CASE if args.rescuarr else CASE)
    report = {"sourceSHA": source_sha,
              "measuredAt": datetime.now(timezone.utc).isoformat(), "fixtureSeed": SEED,
              "platform": {"system": platform.system(), "release": platform.release(), "arch": platform.machine()},
              "scenarios": {scenario: result}}
    try:
        exercise(args, result, source_sha)
    except scenarios.LabFailure as failure:
        result["reason"] = str(failure)
    except (OSError, RuntimeError, ValueError, KeyError, TypeError) as failure:
        result["reason"] = type(failure).__name__
    finally:
        with args.report.open("x", encoding="utf-8") as destination:
            json.dump(report, destination, indent=2)
            destination.write("\n")
    print(json.dumps(result, indent=2), flush=True)
    return int(result["status"] != "observed")


if __name__ == "__main__":
    sys.exit(main())
