"""Observe Sonarr's native queue and search effects for a completed empty pack."""

import json
import os
import shutil
import subprocess
import time

from scenarios import (
    BLOCKLIST_KEYS,
    CATEGORY,
    INDEXER_NAME,
    SOURCE_KEYS,
    call_json,
    completed_fixture_media,
    completed_job,
    delete_queue,
    get_json,
    history_data,
    inventory,
    linked_library_path,
    poll,
    projection,
    sab_job,
    safe_download_path,
    search_events,
    verify,
)
from source import EMPTY_PACK_PHASE, EMPTY_PACK_TARGET, SONARR_TITLE, SOURCE, releases

EMPTY_WARNING = "No files found are eligible for import in "
RETAIN_FLAGS = {"removeFromClient": "false", "blocklist": "true", "skipRedownload": "true", "changeCategory": "false"}


def scoped_history(arr, download_id, title, series_id, target_ids, event):
    rows = [row for row in arr.history(downloadId=download_id) if row.get("eventType") == event]
    if not rows:
        return None
    verify(all(row.get("downloadId") == download_id and row.get("sourceTitle") == title
               and row.get("seriesId") == series_id and row.get("episodeId") in target_ids for row in rows),
           "packHistoryScopeMismatch")
    verify(len(rows) <= len(target_ids) and len({row["episodeId"] for row in rows}) == len(rows),
           "ambiguousPackHistory")
    if len(rows) != len(target_ids):
        return None
    return sorted(rows, key=lambda row: row["episodeId"])


def pack_grab(arr, series_id, target_ids, title):
    for episode_id in target_ids:
        history = [row for row in arr.history(episodeId=episode_id)
                   if row.get("eventType") == "grabbed" and row.get("sourceTitle") == title]
        verify(len(history) <= 1, "ambiguousPackGrab")
        if history and history[0].get("downloadId"):
            return scoped_history(arr, history[0]["downloadId"], title, series_id, target_ids, "grabbed")
    return None


def no_files_queue(arr, download_id, output, series_id, target_ids):
    rows = arr.queue(download_id)
    if not rows:
        return None
    verify(all(row.get("seriesId") == series_id and row.get("episodeId") in target_ids
               and row.get("protocol") == "usenet" for row in rows)
           and len({row["id"] for row in rows}) == len(rows) == len({row["episodeId"] for row in rows}),
           "packQueueScopeMismatch")
    if len(rows) != len(target_ids):
        return None
    if not all(row.get("status") == "completed" and row.get("trackedDownloadState") == "importPending"
               for row in rows):
        return None
    for row in rows:
        verify(safe_download_path(row["outputPath"]) == output, "unexpectedPackOutputPath")
        lines = [line for status in row.get("statusMessages", []) for line in status.get("messages", [])]
        if not any(line.startswith(EMPTY_WARNING) and safe_download_path(line[len(EMPTY_WARNING):]) == output
                   for line in lines):
            return None
    return sorted(rows, key=lambda row: row["episodeId"])


def rejected_pack(arr, media, download_id, title, targets, quality, previous_blocklist):
    # Recheck all aliases by download ID after refresh, regardless of their old row IDs.
    if arr.queue(download_id):
        return None
    history = scoped_history(arr, download_id, title, media["seriesId"], targets, "downloadFailed")
    blocklist = [row for row in arr.blocklist(media) if row["id"] not in previous_blocklist]
    verify(len(blocklist) <= 1, "ambiguousPackBlocklist")
    if not history or not blocklist:
        return None
    row = blocklist[0]
    verify(row.get("seriesId") == media["seriesId"] and sorted(row.get("episodeIds", [])) == targets
           and row.get("sourceTitle") == title and row.get("protocol") == "usenet"
           and row.get("indexer") == INDEXER_NAME and row.get("quality") == quality,
           "packBlocklistScopeMismatch")
    return {
        "failureHistory": [projection(item, SOURCE_KEYS) for item in history],
        "blocklist": projection(row, BLOCKLIST_KEYS),
        "arrQueue": [],
    }


def episode_disposition(arr, series_id, target_ids):
    records = arr.read("episode", seriesId=series_id)
    records = [row for row in records if row.get("id") in target_ids]
    verify(sorted(row["id"] for row in records) == target_ids, "episodeCoverageIncomplete")
    return {row["id"]: episode_file_link(row, series_id) for row in records}


def episode_file_link(row, series_id):
    link = row.get("episodeFileId")
    verify(row.get("seriesId") == series_id and row.get("monitored") is True
           and type(link) is int and link >= 0 and row.get("hasFile") is (link != 0),
           "episodeDispositionInvalid")
    return link


def imported_file(arr, media, download_id, title):
    library = arr.library_file(media["id"])
    if not library:
        return None
    history = scoped_history(arr, download_id, title, media["seriesId"], [media["id"]], "downloadFolderImported")
    if not history:
        return None
    row = history[0]
    verify(str(history_data(row, "fileId")) == str(library["id"]), "packReplacementFileLinkMismatch")
    path = linked_library_path(library["path"], arr.library)
    installed = inventory(path)
    expected = get_json(SOURCE, "/lab/media")
    verify(len(installed) == 1 and projection(installed[0], ("bytes", "sha256")) == expected,
           "packReplacementFileBytesMismatch")
    return {"file": library, "history": projection(row, SOURCE_KEYS), "inventory": installed[0]}


def grab_episode(arr, media, target, phase):
    call_json(SOURCE, f"/lab/feed/{target}/{phase}", method="POST")
    start = len(search_events())
    command, grab = arr.grab(target, media["id"], phase)
    return {"command": command, "searches": arr.search_scope(start, target), "grab": grab}


def run_go_adapter(arr, operation, scope, **fields):
    request = {"operation": operation, "apiKey": arr.headers["X-Api-Key"], "scope": scope, **fields}
    try:
        result = subprocess.run(
            ["/lab-tools/go-pack-probe"], input=json.dumps(request),
            capture_output=True, text=True, timeout=30, check=False,
        )
        proof = json.loads(result.stdout)
    except (OSError, subprocess.TimeoutExpired, ValueError):
        verify(False, "goPackProbeFailed")
    verify(isinstance(proof, dict), "goPackProbeFailed")
    if result.returncode != 0 or proof.get("ok") is not True:
        reasons = {"qualificationUnavailable", "invalidScope", "unverifiedReadback", "uncertainDispatch", "failed"}
        allowed = {f"{stage}:{reason}" for stage in ("preflight", "reject", "readback", "search") for reason in reasons}
        reason = proof.get("reason")
        verify(isinstance(reason, str), "goPackProbeFailed")
        verify(False, f"goPackProbe:{reason}" if reason in allowed else "goPackProbeFailed")
    return proof


def empty_pack_replacement(arr, missing, covered, report, *, go_adapter=False):
    series_id = missing["seriesId"]
    targets = sorted([missing["id"], covered["id"]])
    verify(covered["seriesId"] == series_id and missing["seasonNumber"] == covered["seasonNumber"] == 1
           and len(set(targets)) == 2
           and not inventory(arr.library, all_files=True)
           and not any(episode_disposition(arr, series_id, targets).values()), "unexpectedPackStartingState")

    title = releases(EMPTY_PACK_TARGET, EMPTY_PACK_PHASE)
    call_json(SOURCE, f"/lab/feed/{EMPTY_PACK_TARGET}/{EMPTY_PACK_PHASE}", method="POST")
    start = len(search_events())
    command = arr.command_status("SeasonSearch", seriesId=series_id, seasonNumber=1)
    grab = poll(lambda: pack_grab(arr, series_id, targets, title), "packGrabHistoryMissing")
    download_id = grab[0]["downloadId"]
    report["original"] = {
        "command": command, "searches": arr.search_scope(start, EMPTY_PACK_TARGET),
        "grabs": [projection(item, SOURCE_KEYS) for item in grab],
    }
    job = poll(lambda: completed_job(download_id), "packSABCompletionTimeout", timeout=150)
    verify(job.get("category") == CATEGORY["sonarr"] and job.get("name") == title, "packSABIdentityMismatch")
    expected = get_json(SOURCE, "/lab/media")
    # Move only the byte-verified disposable clip from this exact SAB job.
    output, original_file = completed_fixture_media(job["storage"], title, expected, report)
    # Stage a separate file the native scanner can bind to the already-covered
    # episode. An indexer grab of equal quality could be rejected while the
    # original pack remains queued, which would not test a real library link.
    series = arr.read(f"series/{series_id}")
    verify(series.get("id") == series_id and series.get("monitored") is True, "packSeriesScopeChanged")
    library_series = linked_library_path(series["path"], arr.library)
    season = library_series / "Season 01"
    season.mkdir(parents=True, exist_ok=True)
    for path in (library_series, season):
        os.chown(path, 1000, 1000)
        path.chmod(0o775)
    staged = season / f"{SONARR_TITLE} - S01E02 - Episode 2 WEBDL-720p.mkv"
    with staged.open("xb") as destination, original_file.open("rb") as source:
        shutil.copyfileobj(source, destination)
    original_file.unlink()
    verify(not inventory(output, all_files=True), "syntheticPackNotEmpty")
    rescan = arr.command_status("RescanSeries", seriesId=series_id)
    existing_file = poll(lambda: arr.library_file(covered["id"]), "existingEpisodeLibraryLinkMissing")
    verify(linked_library_path(existing_file["path"], arr.library) == staged
           and len(inventory(staged)) == 1 and projection(inventory(staged)[0], ("bytes", "sha256")) == expected,
           "existingEpisodeFileMismatch")
    before_links = episode_disposition(arr, series_id, targets)
    verify(before_links == {missing["id"]: 0, covered["id"]: existing_file["id"]},
           "packEpisodeDispositionChanged")
    report["alreadyCovered"] = {"command": rescan, "file": existing_file, "inventory": inventory(staged)[0]}

    report["settings"]["importReadback"] = arr.settings(True)
    arr.refresh()

    rows = poll(lambda: no_files_queue(arr, download_id, output, series_id, targets), "packNoEligibleFilesNotObserved")
    client = sab_job(download_id)
    verify(not client["queue"] and len(client["history"]) == 1
           and client["history"][0].get("status") == "Completed", "packSABHistoryMissing")
    previous_blocklist = {item["id"] for item in arr.blocklist(missing)}
    verify(not scoped_history(arr, download_id, title, series_id, targets, "downloadFailed"), "packAlreadyFailed")
    verify(not any(row.get("eventType") == "downloadFolderImported" for row in arr.history(downloadId=download_id)),
           "packAlreadyImported")
    before_library = inventory(arr.library, all_files=True)
    candidates = arr.read("manualimport", downloadId=download_id, filterExistingFiles="false")
    verify(candidates == [], "packManualImportNotEmpty")
    report["beforeReject"] = {
        "queue": [projection(item, SOURCE_KEYS) for item in rows],
        "reason": "noEligibleFiles", "sourceInventory": [], "manualImportCandidateCount": 0, "client": client,
        "episodeFileIDs": before_links,
    }
    start = len(search_events())
    queue_id = min(row["id"] for row in rows) if go_adapter else rows[0]["id"]
    report["operation"] = {"method": "DELETE", "queueId": queue_id, "query": RETAIN_FLAGS}
    if go_adapter:
        scope = {"QueueIDs": [row["id"] for row in rows], "DownloadID": download_id,
                 "Release": title, "SeriesID": series_id, "EpisodeIDs": targets}
        removal = run_go_adapter(arr, "reject", scope)
        verify(isinstance(removal.get("sentAt"), str), "goPackMissingSendTimestamp")
        report["response"] = {"acknowledged": True}
        report["goAdapter"] = {"preflight": True, "reject": True}
    else:
        report["response"] = delete_queue(arr, rows[0], RETAIN_FLAGS, False)
    arr.refresh()
    rejected = poll(lambda: rejected_pack(arr, missing, download_id, title, targets, grab[0]["quality"], previous_blocklist),
                    "packRejectReadbackMissing")
    time.sleep(15)
    arr.refresh()
    verify(not arr.queue(download_id), "rejectedPackReturnedToQueue")
    verify(search_events()[start:] == [], "packTriggeredImplicitSearch")
    verify(sab_job(download_id) == client and output.is_dir() and not inventory(output, all_files=True)
           and inventory(arr.library, all_files=True) == before_library, "packRejectChangedIndependentState")
    report["afterReject"] = {**rejected, "client": client, "sourceInventory": [], "searchRequests": []}
    if go_adapter:
        proof = run_go_adapter(arr, "verify", scope, sentAt=removal["sentAt"])
        verify(proof.get("verified") is True, "goPackRejectReadbackMissing")
        report["goAdapter"]["verifiedRejection"] = True

    # The existing episode must remain covered and the requested episode still missing.
    verify(episode_disposition(arr, series_id, targets) == before_links, "packEpisodeDispositionChanged")
    if go_adapter:
        call_json(SOURCE, "/lab/feed/episode1/replacement", method="POST")
        search_start = len(search_events())
        proof = run_go_adapter(arr, "search", scope, episodeIds=[missing["id"]])
        command_id = proof.get("commandId")
        verify(type(command_id) is int and command_id > 0, "goPackMissingSearchCommand")
        command = poll(lambda: arr.completed_command(command_id, "EpisodeSearch"), "goPackSearchCompletionMissing")
        grabbed = poll(lambda: arr.find_grab(missing["id"], "episode1", "replacement"), "goPackReplacementGrabMissing")
        replacement = {"command": command, "searches": arr.search_scope(search_start, "episode1"), "grab": grabbed}
        report["goAdapter"]["searchCommandId"] = command_id
    else:
        replacement = grab_episode(arr, missing, "episode1", "replacement")
    new_id, new_title = replacement["grab"]["downloadId"], replacement["grab"]["sourceTitle"]
    verify(new_id != download_id, "replacementReusedExistingDownload")
    poll(lambda: completed_job(new_id), "packReplacementSABCompletionTimeout", timeout=150)
    arr.refresh()
    repaired_file = poll(lambda: imported_file(arr, missing, new_id, new_title), "packReplacementImportMissing")
    replacement["grab"] = projection(replacement["grab"], SOURCE_KEYS)
    report["replacement"] = {**replacement, **repaired_file, "episodeIds": [missing["id"]]}
    verify(arr.library_file(covered["id"]) == existing_file and not arr.queue(download_id)
           and sab_job(download_id) == client and output.is_dir() and not inventory(output, all_files=True),
           "packReplacementChangedIndependentState")
