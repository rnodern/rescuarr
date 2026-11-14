"""Observe scoped Sonarr library-file removal and replacement in the isolated lab."""

from empty_pack import episode_disposition, grab_episode, imported_file, pack_grab
from scenarios import (
    CATEGORY,
    PAGE,
    SOURCE_KEYS,
    call_json,
    completed_job,
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
from source import EMPTY_PACK_TARGET, LANGUAGE_PACK_PHASE, SOURCE, article_filename, releases


def checked_file(arr, episode, expected, language):
    file_id = arr.file_link(episode["id"])
    if not file_id:
        return None
    row = arr.read(f"episodefile/{file_id}")
    info = row.get("mediaInfo") or {}
    path = linked_library_path(row["path"], arr.library)
    contents = inventory(path)
    verify(row.get("id") == file_id and row.get("seriesId") == episode["seriesId"]
           and row.get("seasonNumber") == episode["seasonNumber"]
           and len(contents) == 1 and row.get("size") == expected["bytes"]
           and projection(contents[0], ("bytes", "sha256")) == expected, "languagePackFileIdentityMismatch")
    verify(info.get("audioStreamCount") == 1 and info.get("audioLanguages") == language,
           "languagePackAudioTagsMismatch")
    return {**projection(row, ("id", "seriesId", "seasonNumber", "path", "size", "dateAdded", "languages")),
            "mediaInfo": projection(info, ("audioStreamCount", "audioLanguages")), "inventory": contents[0]}


def pack_import_history(arr, download_id, files, source):
    imports = [row for row in arr.history(downloadId=download_id) if row.get("eventType") == "downloadFolderImported"]
    verify(len(imports) <= len(files), "languagePackImportHistoryAmbiguous")
    if len(imports) != len(files):
        return None
    verify(set(files) == {row.get("episodeId") for row in imports}, "languagePackImportHistoryScopeMismatch")
    evidence = []
    for row in sorted(imports, key=lambda item: item["episodeId"]):
        file = files[row["episodeId"]]
        dropped = safe_download_path(history_data(row, "droppedPath"))
        target = source[row["episodeId"]]
        verify(row.get("downloadId") == download_id and row.get("seriesId") == file["seriesId"]
               and str(history_data(row, "fileId")) == str(file["id"])
               and history_data(row, "importedPath") == file["path"] and dropped == target,
               "languagePackImportHistoryScopeMismatch")
        evidence.append({**projection(row, SOURCE_KEYS), "fileId": file["id"], "droppedPath": str(dropped),
                         "importedPath": file["path"]})
    return evidence


def exact_deletion(arr, episode, file, before_ids):
    new_events = [row for row in arr.history(episodeId=episode["id"])
                  if row.get("id") not in before_ids and row.get("eventType") == "episodeFileDeleted"]
    if not new_events:
        return None
    verify(len(new_events) == 1, "languagePackDeletionHistoryAmbiguous")
    event = new_events[0]
    verify(event.get("seriesId") == episode["seriesId"] and event.get("episodeId") == episode["id"]
           and event.get("sourceTitle") == file["path"] and history_data(event, "reason") == "Manual",
           "languagePackDeletionHistoryScopeMismatch")
    return {**projection(event, SOURCE_KEYS), "reason": "Manual"}


def language_pack_replacement(arr, wrong, retained, report):
    series_id = wrong["seriesId"]
    wrong_id, retained_id = wrong["id"], retained["id"]
    targets = sorted((wrong_id, retained_id))
    verify(wrong_id != retained_id and retained["seriesId"] == series_id
           and wrong["episodeNumber"] == 1 and retained["episodeNumber"] == 2
           and wrong["seasonNumber"] == retained["seasonNumber"] == 1
           and not inventory(arr.library, all_files=True)
           and not any(episode_disposition(arr, series_id, targets).values()), "languagePackUnexpectedStartingState")
    settings = arr.read("config/mediamanagement")
    fields = ("enableMediaInfo", "autoUnmonitorPreviouslyDownloadedEpisodes", "recycleBin")
    verify(settings.get("enableMediaInfo") is True
           and settings.get("autoUnmonitorPreviouslyDownloadedEpisodes") is False
           and settings.get("recycleBin") in (None, ""), "languagePackStorageSettingsUnsupported")
    report["settings"]["mediaManagement"] = projection(settings, fields)
    hebrew = get_json(SOURCE, "/lab/media/hebrew")
    english = get_json(SOURCE, "/lab/media")
    verify(hebrew != english, "languagePackFixturesIdentical")

    title = releases(EMPTY_PACK_TARGET, LANGUAGE_PACK_PHASE)
    call_json(SOURCE, f"/lab/feed/{EMPTY_PACK_TARGET}/{LANGUAGE_PACK_PHASE}", method="POST")
    start = len(search_events())
    command = arr.command_status("SeasonSearch", seriesId=series_id, seasonNumber=1)
    grabs = poll(lambda: pack_grab(arr, series_id, targets, title), "languagePackGrabMissing")
    download_id = grabs[0]["downloadId"]
    report["original"] = {"command": command, "searches": arr.search_scope(start, EMPTY_PACK_TARGET),
                          "grabs": [projection(row, SOURCE_KEYS) for row in grabs]}
    job = poll(lambda: completed_job(download_id), "languagePackSABCompletionMissing", timeout=150)
    verify(job.get("category") == CATEGORY["sonarr"] and job.get("name") == title, "languagePackSABScopeMismatch")
    output = safe_download_path(job["storage"])
    verify(output.name == title, "languagePackSABPathMismatch")
    source = {episode_id: output / article_filename(target, LANGUAGE_PACK_PHASE)
              for episode_id, target in ((wrong_id, "episode1"), (retained_id, "episode2"))}
    arr.refresh()
    wrong_file = poll(lambda: checked_file(arr, wrong, hebrew, "heb"), "languagePackWrongFileImportMissing")
    correct_file = poll(lambda: checked_file(arr, retained, english, "eng"), "languagePackCorrectFileImportMissing")
    files = {wrong_id: wrong_file, retained_id: correct_file}
    report["original"]["files"] = files
    report["original"]["importHistory"] = poll(lambda: pack_import_history(arr, download_id, files, source),
                                               "languagePackImportHistoryMissing")

    # A file can represent multiple episodes. Check all series links before the exact DELETE.
    rows = arr.read("episode", seriesId=series_id)
    verify({row["id"] for row in rows if row.get("episodeFileId") == wrong_file["id"]} == {wrong_id}
           and {row["id"] for row in rows if row.get("episodeFileId") == correct_file["id"]} == {retained_id}
           and checked_file(arr, wrong, hebrew, "heb") == wrong_file, "languagePackDeleteScopeChanged")
    previous = {row["id"] for row in arr.history(episodeId=wrong_id)}
    guard = {row["id"] for row in arr.history(episodeId=retained_id)}
    client = sab_job(download_id)
    start = len(search_events())
    report["deletion"] = {"method": "DELETE", "path": f"/api/v3/episodefile/{wrong_file['id']}"}
    status, _ = arr.api(f"episodefile/{wrong_file['id']}", method="DELETE")
    deletion = poll(lambda: exact_deletion(arr, wrong, wrong_file, previous), "languagePackDeletionHistoryMissing")
    installed = arr.read("episodefile", seriesId=series_id)
    verify({row["id"] for row in installed} == {correct_file["id"]}
           and not linked_library_path(wrong_file["path"], arr.library).exists()
           and checked_file(arr, retained, english, "eng") == correct_file
           and {row["id"] for row in arr.history(episodeId=retained_id)} == guard
           and sab_job(download_id) == client and search_events()[start:] == [], "languagePackDeletionEffectsMismatch")
    report["deletion"].update(httpStatus=status, history=deletion, retained=correct_file,
                              retainedHistoryIDs=sorted(guard), libraryPathAbsent=True, sourceJobUnchanged=True)

    # Search only a currently monitored unsatisfied episode with no competing queue entry.
    verify(arr.read(f"series/{series_id}").get("monitored") is True, "languagePackSeriesUnmonitored")
    links = episode_disposition(arr, series_id, targets)
    verify(links == {wrong_id: 0, retained_id: correct_file["id"]}, "languagePackPostDeleteDispositionChanged")
    queue = arr.read("queue", **PAGE, sortKey="added", **{arr.unknown_flag: "true"})
    verify(queue.get("totalRecords") == len(queue.get("records", []))
           and not any(row.get("seriesId") == series_id and row.get("episodeId") in (None, 0, wrong_id)
                       for row in queue["records"]), "languagePackReplacementHasCompetingQueue")
    report["deletion"]["episodeFileIDs"] = links
    replacement = grab_episode(arr, wrong, "episode1", "replacement")
    replacement_id = replacement["grab"]["downloadId"]
    verify(replacement_id != download_id, "languagePackReusedOriginalJob")
    poll(lambda: completed_job(replacement_id), "languagePackReplacementSABCompletionMissing", timeout=150)
    arr.refresh()
    repaired = poll(lambda: imported_file(arr, wrong, replacement_id, replacement["grab"]["sourceTitle"]),
                    "languagePackReplacementImportMissing")
    new_file = checked_file(arr, wrong, english, "eng")
    verify(new_file["id"] != wrong_file["id"] and new_file["id"] == repaired["file"]["id"]
           and checked_file(arr, retained, english, "eng") == correct_file
           and {row["id"] for row in arr.history(episodeId=retained_id)} == guard,
           "languagePackReplacementChangedUnrelatedState")
    report["replacement"] = {**replacement, "grab": projection(replacement["grab"], SOURCE_KEYS),
                             "episodeIds": [wrong_id], "file": new_file, "history": repaired["history"]}
