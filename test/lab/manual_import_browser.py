"""Observe a packaged owner-approved Copy against the disposable Sonarr fixture."""

import hashlib
import http.client
import json
import platform
import tempfile
import threading
import time
from contextlib import ExitStack
from functools import partial
from html import unescape
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from urllib.parse import urlsplit

from manual_browser import PackagedManualLab, PostForms, REVIEW, require_ready, start_process, stop_process
from scenarios import LabFailure, id_only_import, inventory, poll, projection, sab_job, search_events, verify

COMMAND = "/api/v3/command"
REPROCESS = "/api/v3/manualimport"
PRESERVATION_NOTICE = "Keep the source file and the download client job."
SONARR_CLEANUP_NOTICE = ("Sonarr uses a hardlink when the storage supports it and copies otherwise. Its completed "
                         "download handling can remove the entire download folder and the job from "
                         "SABnzbd’s active history.")
PRESERVATION_ACTIVITY = "Copy import verified. Sonarr linked the approved episode and kept the source file."
SONARR_CLEANUP_ACTIVITY = "Import verified. Sonarr linked the approved episode. Sonarr manages download cleanup."


def capture_sonarr(stack, arr):
    """Proxy the app's API traffic and retain synthetic write evidence in memory."""
    upstream = urlsplit(arr.origin)
    verify(upstream.scheme == "http" and upstream.netloc in ("sonarr:8989", "127.0.0.1:18989")
           and not (upstream.path or upstream.query or upstream.fragment), "manualCopyUpstreamOutsideLab")
    writes = []
    lock = threading.Lock()

    class BrowserArrHandler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def exchange(self):
            if not self.path.startswith("/api/v3/"):
                self.send_error(404)
                return
            method = self.command
            route = urlsplit(self.path).path
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 <= length <= 256_000:
                self.send_error(413)
                return
            payload = self.rfile.read(length) if length else None
            record = None
            if method != "GET":
                record = {"method": method, "route": route}
                if (method, route) in (("POST", COMMAND), ("POST", REPROCESS)):
                    try:
                        record["request"] = json.loads(payload)
                    except (ValueError, TypeError):
                        pass
                with lock:
                    writes.append(record)
                if (method, route) not in (("POST", COMMAND), ("POST", REPROCESS)):
                    self.send_error(405)
                    return
            headers = {name: self.headers[name] for name in ("Accept", "Content-Type", "X-Api-Key")
                       if name in self.headers}
            connection = http.client.HTTPConnection(upstream.hostname, upstream.port, timeout=15)
            try:
                connection.request(method, self.path, body=payload, headers=headers)
                response = connection.getresponse()
                with response:
                    body = response.read(4_000_001)
                    if len(body) > 4_000_000:
                        self.send_error(502)
                        return
                    if record is not None:
                        with lock:
                            record["status"] = response.status
                            if route == COMMAND and response.status in (200, 201, 202):
                                try:
                                    record["commandId"] = json.loads(body).get("id")
                                except (ValueError, AttributeError):
                                    pass
                    self.send_response(response.status)
                    self.send_header("Content-Type", response.getheader("Content-Type", "application/json"))
                    self.send_header("Content-Length", str(len(body)))
                    self.end_headers()
                    self.wfile.write(body)
            except (OSError, http.client.HTTPException):
                self.close_connection = True
            finally:
                connection.close()

        do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = exchange

    server = stack.enter_context(ThreadingHTTPServer(("127.0.0.1", 18986), BrowserArrHandler))

    def snapshot():
        with lock:
            return [record.copy() for record in writes]

    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    stack.callback(server.shutdown)
    app_connection = SimpleNamespace(origin=f"http://127.0.0.1:{server.server_port}", headers=arr.headers)
    return app_connection, snapshot


def copy_review(browser, title, folder, source, *, approval_label="Approve Copy import", sonarr_cleanup=False, tagged_audio=False):
    hold = "pending"
    notice = SONARR_CLEANUP_NOTICE if sonarr_cleanup else PRESERVATION_NOTICE

    def checked():
        nonlocal hold
        page = browser.get(REVIEW)
        verify("<h1>Manual Review</h1>" in page, "manualCopyReviewPageMissing")
        forms = PostForms(page)
        decline = forms.form(REVIEW, decision="decline")
        approval = forms.form(REVIEW, decision="approve")
        if not decline or not approval:
            reasons = {"no downloads are waiting": "caseNotOffered",
                       "did not confirm one import candidate": "candidate",
                       "configured audio policy rejects": "audioPolicy",
                       "waiting for its first library file": "target",
                       "qualified": "qualification",
                       "cannot confirm": "evidence"}
            hold = next((category for marker, category in reasons.items() if marker in page.lower()), "unknown")
            return None
        verify(title in page and folder.name in page and source.name in page and "S01E01" in page
               and approval_label in page
               and notice in " ".join(unescape(page).split()), "manualCopyReviewEvidenceIncomplete")
        verify(bool(decline.get("token")) and bool(approval.get("token")), "manualCopyReviewTokenMissing")
        if tagged_audio:
            verify("<dt>Audio tracks</dt><dd>English</dd>" in page and "original audio present" in page,
                   "manualCopySourceAudioNotConfirmed")
        return decline, approval
    try:
        return poll(checked, "manualCopyReviewDeadline", timeout=60)
    except LabFailure as failure:
        if str(failure) != "manualCopyReviewDeadline":
            raise
        raise LabFailure(f"manualCopyReviewDeadline:{hold}") from None


def prove_reprocess(writes, download_id, source, media):
    for item in writes:
        verify((item["method"], item["route"]) == ("POST", REPROCESS), "manualCopySentBeforeApproval")
        request = item.get("request")
        verify(isinstance(request, list) and len(request) == 1 and isinstance(request[0], dict),
               "manualCopyUnexpectedReprocessShape")
        candidate = request[0]
        verify(candidate.get("path") == str(source) and candidate.get("downloadId") == download_id
               and candidate.get("seriesId") == media["seriesId"] and candidate.get("episodeIds") == [media["id"]],
               "manualCopyUnexpectedReprocessScope")
    return len(writes)


def prove_copy_command(writes, download_id, source, media):
    effects = [record for record in writes if record["route"] != REPROCESS]
    verify(len(effects) == 1 and (effects[0]["method"], effects[0]["route"]) == ("POST", COMMAND),
           "manualCopyUnexpectedEffectCount")
    record = effects[0]
    request = record.get("request")
    verify(isinstance(request, dict) and request.get("name") == "ManualImport"
           and request.get("importMode") == "copy", "manualCopyWrongCommand")
    files = request.get("files")
    verify(isinstance(files, list) and len(files) == 1 and isinstance(files[0], dict), "manualCopyUnexpectedFileCount")
    file = files[0]
    verify(file.get("path") == str(source) and file.get("downloadId") == download_id
           and file.get("seriesId") == media["seriesId"] and file.get("episodeIds") == [media["id"]],
           "manualCopyUnexpectedCommandScope")
    command_id = record.get("commandId")
    verify(type(command_id) is int and command_id > 0, "manualCopyCommandAcknowledgementMissing")
    return command_id


def run_copy_browser(lab, arr, download_id, title, folder, source, media, guard, report, *, sonarr_cleanup=False):
    app_connection, captured = capture_sonarr(lab.stack, arr)

    def native_state():
        keys = ("id", "seriesId", "episodeId", "status", "trackedDownloadState", "statusMessages")
        return {
            "queue": [projection(row, keys) for row in arr.queue(download_id)],
            "historyIds": sorted(row["id"] for row in arr.history(downloadId=download_id)),
            "guardHistoryIds": sorted(row["id"] for row in arr.history(episodeId=guard["id"])),
            "fileLinks": [arr.file_link(media["id"]), arr.file_link(guard["id"])],
            "source": inventory(folder, all_files=True),
            "library": inventory(arr.library, all_files=True),
            "client": sab_job(download_id),
            "searches": search_events(),
        }

    before = native_state()
    lab.on_stage("manualCopyDecline")
    proc, browser, _ = lab.setup_manual_review(app_connection, lab.root / "decline", operation="manual-import")
    decline, _ = copy_review(browser, title, folder, source, approval_label="I checked the video and audio. Approve",
                             sonarr_cleanup=sonarr_cleanup)
    browser.post(REVIEW, REVIEW, decline)
    browser.activity("Manual recovery declined")
    verify(PostForms(browser.get(REVIEW)).form(REVIEW, decision="approve") is None, "manualCopyDeclinedCaseOfferedAgain")
    lab.stop(proc)
    verify(native_state() == before, "manualCopyDeclineChangedUpstream")
    verify(not captured(), "manualCopySentBeforeDeclineDecision")

    lab.on_stage("manualCopyProfile")
    data = lab.root / "approve"
    audio_settings = ({
        "RESCUARR_MANUAL_IMPORT_AUDIO_SONARR_ROOT": str(folder.parent),
        "RESCUARR_MANUAL_IMPORT_AUDIO_LOCAL_ROOT": str(folder.parent),
    } if sonarr_cleanup else None)
    proc, browser, settings = lab.setup_manual_review(app_connection, data, operation="manual-import",
                                                       allow_unknown=not sonarr_cleanup, extra_settings=audio_settings)
    decline, _ = copy_review(browser, title, folder, source, sonarr_cleanup=sonarr_cleanup, tagged_audio=sonarr_cleanup)
    lab.stop(proc)
    proc, browser = lab.start(data, settings)
    browser.login()
    restored, approval = copy_review(browser, title, folder, source, sonarr_cleanup=sonarr_cleanup,
                                     tagged_audio=sonarr_cleanup)
    verify(restored["token"] == decline["token"], "manualCopyReviewDidNotSurviveRestart")
    time.sleep(6)
    verify(native_state() == before, "manualCopyChangedUpstreamBeforeApproval")
    verify(not captured(), "manualCopySentBeforeApproval")

    lab.on_stage("manualCopyApprove")
    browser.post(REVIEW, REVIEW, approval)
    activity = SONARR_CLEANUP_ACTIVITY if sonarr_cleanup else PRESERVATION_ACTIVITY
    browser.activity(activity, timeout=90)
    lab.stop(proc)
    if sonarr_cleanup:
        proc, browser = lab.start(data, settings)
        browser.login()
        browser.activity(activity)
        lab.stop(proc)
    writes = captured()
    reprocess_count = prove_reprocess([record for record in writes if record["route"] == REPROCESS], download_id, source, media)
    verify(reprocess_count == 1, "manualCopyUnexpectedReprocessCount")
    command_id = prove_copy_command(writes, download_id, source, media)
    command = poll(lambda: arr.completed_command(command_id, "ManualImport"), "manualCopyCompletionMissing")
    verify(search_events() == before["searches"], "manualCopyTriggeredSearch")
    report["operation"] = {"method": "POST", "path": COMMAND, "name": "ManualImport", "importMode": "copy",
                           "fileCount": 1, "downloadId": download_id, "seriesId": media["seriesId"], "episodeIds": [media["id"]]}
    report["manualBrowser"] = {"packagedVersion": lab.version, "declineRecorded": True, "declineMadeNoChanges": True,
                               "reviewSurvivedRestart": True, "noChangesBeforeApproval": True,
                               "apiWritesBeforeApproval": 0, "apiWritesAfterDecline": 0, "reprocessRequests": reprocess_count,
                               "copyCommandId": command_id, "importRecordedInActivity": True}
    if sonarr_cleanup:
        report["manualBrowser"].update(cleanupNoticeShown=True, importActivitySurvivedRestart=True,
                                       taggedSourceAudioConfirmed=True)
    return command


def run_packaged_id_only_copy(arr, media, guard, report, executable, source_sha, on_stage, *, sonarr_cleanup=False):
    report["packagedRuntime"] = {"system": platform.system(), "release": platform.release(), "arch": platform.machine()}
    with executable.open("rb") as binary:
        report["packagedBinary"] = {"name": executable.name, "sha256": hashlib.file_digest(binary, "sha256").hexdigest()}
    config = arr.read("config/mediamanagement")
    verify(type(config.get("id")) is int and config["id"] > 0, "manualCopyMissingMediaManagement")
    route = f"config/mediamanagement/{config['id']}"
    expected = {"copyUsingHardlinks": sonarr_cleanup, "importExtraFiles": False,
                "useScriptImport": False, "enableMediaInfo": True}
    arr.update(route, {**config, **expected})
    saved = arr.read(route)
    verify(projection(saved, expected) == expected, "manualCopyMediaManagementNotApplied")
    report["mediaManagement"] = expected

    with ExitStack() as stack:
        root = Path(stack.enter_context(tempfile.TemporaryDirectory(prefix="rescuarr-manual-copy-"))).resolve()
        lab = PackagedManualLab(executable, root, stack, start_process, require_ready, stop_process, on_stage,
                                None, source_sha)
        id_only_import(arr, media, guard, report, observe_cleanup=sonarr_cleanup,
                       manual_review=partial(run_copy_browser, lab, arr, sonarr_cleanup=sonarr_cleanup))
