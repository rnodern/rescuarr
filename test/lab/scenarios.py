"""Drive only the fixed disposable Arr/SAB products, preserving scoped evidence."""

import hashlib
import http.client
import json
import os
import time
import urllib.error
from pathlib import Path
from urllib.parse import urlencode

from probe import TARGETS as ARR_BUILDS, arr_key, call_json, get_json, sab_key, wait_for
from source import SEED, SOURCE, SONARR_TITLE, TARGETS, TMDB_IDS, TVDB_ID, releases

SAB = "http://sab:8080"
DOWNLOADS = Path("/downloads")
PAGE = {"page": 1, "pageSize": 50, "sortDirection": "descending"}
COMMAND = {"sonarr": "EpisodeSearch", "radarr": "MoviesSearch"}
TARGET_FIELD = {"sonarr": "episodeIds", "radarr": "movieIds"}
HISTORY_TARGET = {"sonarr": "episodeId", "radarr": "movieIds"}
MEDIA_FIELD = {"sonarr": "episodeId", "radarr": "movieId"}
FILE_FIELD = {"sonarr": "episodeFileId", "radarr": "movieFileId"}
PROFILE_NAME = "Rescuarr Lab 720p"
CATEGORY = {"sonarr": "sonarr-lab", "radarr": "radarr-lab"}
SOURCE_KEYS = ("id", "status", "eventType", "sourceTitle", "episodeId", "seriesId", "movieId", "downloadId", "date")
BLOCKLIST_KEYS = ("id", "sourceTitle", "date", "protocol", "indexer", "quality", "seriesId", "episodeIds", "movieId")
CLIENT_NAME = "LAB SAB"
INDEXER_NAME = "LAB Newznab"


class LabFailure(Exception):
    pass


def verify(condition, reason):
    if not condition:
        raise LabFailure(reason)


def poll(observe, reason, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = observe()
        if value:
            return value
        time.sleep(1)
    raise LabFailure(reason)


def projection(value, keys):
    return {key: value[key] for key in keys if key in value}


def inventory(path):
    if not path.exists():
        return []
    files = [path] if path.is_file() else sorted(path.rglob("*.mkv"))
    evidence = []
    for item in files:
        with item.open("rb") as contents:
            evidence.append({"name": item.name, "bytes": item.stat().st_size,
                             "sha256": hashlib.file_digest(contents, "sha256").hexdigest()})
    return evidence


def safe_path(path, root):
    verify(isinstance(path, str) and Path(path).is_absolute(), "sourceOutsideLab")
    resolved = Path(path).resolve()
    verify(resolved != root and resolved.is_relative_to(root), "sourceOutsideLab")
    return resolved


def safe_download_path(path):
    return safe_path(path, DOWNLOADS / "complete")


def linked_library_path(path, lab_library):
    upstream = Path(path)
    verify(upstream.is_relative_to("/library"), "sourceOutsideLab")
    return safe_path(str(lab_library / upstream.relative_to("/library")), lab_library)


def validation_category(error):
    """Record an allowlisted category and discard all upstream text and fields."""
    try:
        body = error.read(8193)
        problems = json.loads(body) if len(body) <= 8192 else None
    except (OSError, ValueError):
        return "validationUnknown"
    if isinstance(problems, dict):
        messages = [problems.get("message", ""), problems.get("description", "")]
        combined = " ".join(value.lower() for value in messages if isinstance(value, str))
        if "not null constraint failed" in combined:
            if "series.malids" in combined or "series.anilistids" in combined:
                return "missingSeriesAnimeIDArrays"
            return "databaseRequiredField"
        if "unique constraint failed" in combined:
            return "databaseDuplicateIdentity"
        if "constraint failed" in combined:
            return "databaseConstraint"
        return "validationUnknown"
    if not isinstance(problems, list):
        return "validationUnknown"
    for problem in problems:
        if not isinstance(problem, dict):
            continue
        message = problem.get("errorMessage", "")
        if not isinstance(message, str):
            continue
        message = message.lower()
        for signature, category in (
            ("no results", "emptyRecentFeed"),
            ("no rss feed query", "missingRecentQuery"),
            ("does not support any of the selected categories", "unsupportedCategory"),
            ("valid url", "invalidURL"),
            ("unable to connect", "cannotConnect"),
        ):
            if signature in message:
                return category
    return "validationUnknown"


class Arr:
    def __init__(self, product):
        self.product = product
        self.port, _, self.unknown_flag = ARR_BUILDS[product]
        self.origin = f"http://{product}:{self.port}"
        self.headers = {"X-Api-Key": arr_key(product)}
        self.command = COMMAND[product]
        self.targets = TARGET_FIELD[product]
        self.file_field = FILE_FIELD[product]
        self.library = Path("/library", product)

    def api(self, path, *, method="GET", query=None, body=None, timeout=5):
        try:
            return call_json(self.origin, "/api/v3/" + path, query, self.headers, method=method, body=body, timeout=timeout)
        except TimeoutError:
            raise LabFailure(f"{method}:{path.split('/')[0]}:timeout") from None
        except urllib.error.HTTPError as error:
            with error:
                route = path.split("/")[0]
                inspect_validation = method == "POST" and ((route == "indexer" and error.code == 400)
                                                           or (route == "series" and error.code in (400, 409)))
                category = validation_category(error) if inspect_validation else ""
            raise LabFailure(f"{method}:{route}:HTTP{error.code}" + (f":{category}" if category else "")) from None

    def read(self, path, **query):
        return self.api(path, query=query)[1]

    def configure(self, path, value, *, timeout=5, **query):
        return self.api(path, method="POST", query=query, body=value, timeout=timeout)[1]

    def update(self, path, value):
        return self.api(path, method="PUT", body=value)[1]

    def command_status(self, name, **params):
        status, command = self.api("command", method="POST", body={"name": name, **params})
        verify(isinstance(command.get("id"), int), "invalidCommandID")
        def terminal():
            state = self.read(f"command/{command['id']}")
            return state if state["status"] in ("completed", "failed", "aborted", "cancelled", "orphaned") else None

        state = poll(terminal, "commandTimeout:" + name)
        verify(state["status"] == "completed", "commandFailed:" + name)
        return {"responseStatus": status, **projection(state, ("id", "name", "status", "result", "started", "ended"))}

    def refresh(self):
        self.command_status("RefreshMonitoredDownloads")

    def settings(self, completed, *, automatic_redownload=True):
        config = self.read("config/downloadclient")
        path = f"config/downloadclient/{config['id']}"
        self.update(path, {
            **config,
            "enableCompletedDownloadHandling": completed,
            "autoRedownloadFailed": automatic_redownload,
            "autoRedownloadFailedFromInteractiveSearch": automatic_redownload,
        })
        config = self.read(path)
        verify(config["enableCompletedDownloadHandling"] is completed, "completedHandlingDidNotApply")
        verify(config["autoRedownloadFailed"] is automatic_redownload
               and config["autoRedownloadFailedFromInteractiveSearch"] is automatic_redownload,
               "automaticRedownloadDidNotApply")
        return projection(config, ("enableCompletedDownloadHandling", "autoRedownloadFailed",
                                   "autoRedownloadFailedFromInteractiveSearch"))

    def provider(self, route, implementation, name, fields, settings):
        schemas = self.read(route + "/schema")
        templates = [item for item in schemas if item["implementation"] == implementation]
        verify(bool(templates), "missingProviderSchema:" + route)
        template = templates[0]
        known = {field["name"] for field in template["fields"]}
        verify(fields.keys() <= known, "unsupportedProviderSetting:" + route)
        template["fields"] = [
            {**field, "value": fields[field["name"]]} if field["name"] in fields else field
            for field in template["fields"]
        ]
        self.configure(route, {**template, **settings, "name": name})

    def setup(self, *, automatic_redownload=True):
        self.settings(False, automatic_redownload=automatic_redownload)
        self.configure("rootfolder", {"path": "/library"})
        profiles = self.read("qualityprofile")
        verify(bool(profiles), "missingQualityProfile")
        profile = profiles[0]
        quality = None
        for item in profile["items"]:
            item["allowed"] = False
            if (item.get("quality") or {}).get("name", "").lower() == "webdl-720p":
                quality = item.get("quality")
                item["allowed"] = True
            for group_item in item.get("items", []):
                group_item["allowed"] = (group_item.get("quality") or {}).get("name", "").lower() == "webdl-720p"
                if group_item["allowed"]:
                    item["allowed"] = True
                    quality = group_item["quality"]
        verify(quality is not None, "missing720pWebDLQuality")
        cutoff = next(item for item in profile["items"] if item["allowed"])
        profile = self.update(f"qualityprofile/{profile['id']}", {
            **profile, "name": PROFILE_NAME, "upgradeAllowed": False,
            "cutoff": cutoff.get("id") or quality["id"],
        })
        definition = next(item for item in self.read("qualitydefinition") if item["quality"]["id"] == quality["id"])
        self.update(f"qualitydefinition/{definition['id']}", {**definition, "minSize": 0})
        self.provider("downloadclient", "Sabnzbd", CLIENT_NAME, {
            "host": "sab", "port": 8080, "apiKey": sab_key(), "useSsl": False,
            "tvCategory" if self.product == "sonarr" else "movieCategory": CATEGORY[self.product],
        }, {"enable": True, "removeCompletedDownloads": False, "removeFailedDownloads": False})
        self.provider("indexer", "Newznab", INDEXER_NAME, {
            "baseUrl": f"{SOURCE}/{self.product}", "apiPath": "/api",
            "categories": [5040 if self.product == "sonarr" else 2040],
        }, {"enableRss": False, "enableAutomaticSearch": True, "enableInteractiveSearch": True})
        return profile

    def media(self, profile_id):
        options = {"qualityProfileId": profile_id, "rootFolderPath": "/library", "monitored": True}
        if self.product == "sonarr":
            series = self.configure("series", {"title": SONARR_TITLE, "tvdbId": TVDB_ID, **options,
                                               "seriesType": "standard", "seasonFolder": True,
                                               "addOptions": {"monitor": "all", "searchForMissingEpisodes": False}}, timeout=45)
            def episodes():
                rows = self.read("episode", seriesId=series["id"])
                return sorted(rows, key=lambda row: row["episodeNumber"]) if len(rows) == 2 else None
            return poll(episodes, "episodeMetadataTimeout", timeout=60)
        return [
            self.configure("movie", {"tmdbId": movie_id, **options, "minimumAvailability": "released",
                                     "addOptions": {"searchForMovie": False}})
            for movie_id in TMDB_IDS
        ]

    def queue(self, download_id):
        records = self.read("queue", **PAGE, sortKey="added", **{self.unknown_flag: "true"})
        verify(records["totalRecords"] <= PAGE["pageSize"], "queueCoverageIncomplete")
        return [row for row in records["records"] if row.get("downloadId") == download_id]

    def history(self, **query):
        result = self.read("history", **PAGE, sortKey="date", **query)
        verify(result["totalRecords"] <= PAGE["pageSize"], "historyCoverageIncomplete")
        return result["records"]

    def blocklist(self, target):
        key = "seriesIds" if self.product == "sonarr" else "movieIds"
        record_id = target["seriesId"] if self.product == "sonarr" else target["id"]
        result = self.read("blocklist", **PAGE, sortKey="date", protocols="usenet", **{key: record_id})
        verify(result["totalRecords"] <= PAGE["pageSize"], "blocklistCoverageIncomplete")
        return result["records"]

    def grab(self, target_name, target_id, phase):
        command = self.command_status(self.command, **{self.targets: [target_id]})
        row = poll(lambda: self.find_grab(target_id, target_name, phase), "grabHistoryMissing:" + target_name)
        return command, row

    def find_grab(self, target_id, target_name, phase):
        query = {HISTORY_TARGET[self.product]: target_id}
        expected = releases(target_name, phase)
        matches = [row for row in self.history(**query) if row["eventType"] == "grabbed"
                   and row.get(MEDIA_FIELD[self.product]) == target_id
                   and row["sourceTitle"] == expected and row.get("downloadId")]
        verify(len(matches) <= 1, "duplicatedGrabHistory")
        return matches[0] if matches else None

    def file_link(self, target_id):
        path = "episode" if self.product == "sonarr" else "movie"
        return self.read(f"{path}/{target_id}").get(self.file_field) or 0

    def library_file(self, target_id):
        file_id = self.file_link(target_id)
        if not file_id:
            return None
        path = "episodefile" if self.product == "sonarr" else "moviefile"
        record = self.read(f"{path}/{file_id}")
        return projection(record, ("id", "path", "relativePath", "size", "quality", "languages"))

    def search_scope(self, start_index, expected):
        searches = search_events()[start_index:]
        expected_product = TARGETS[expected][0]
        exact = [event for event in searches if event["product"] == expected_product]
        verify(exact and all(event["targets"] == [expected] for event in exact), "searchScopeMismatch")
        return exact


def search_events():
    return get_json(SOURCE, "/lab/searches")


def sab_api(mode, **query):
    response = get_json(SAB, "/api", {"mode": mode, "output": "json", "apikey": sab_key(), **query})
    verify(response.get("status") is not False, "SABRejected:" + mode)
    return response


def configure_sab():
    # The entrypoint owns configuration and generated API credentials. Restrict its
    # two categories and its only NNTP server to the internal network.
    for setting, value in {"download_dir": "/downloads/incomplete", "complete_dir": "/downloads/complete"}.items():
        sab_api("set_config", section="misc", keyword=setting, value=value)
    for category in CATEGORY.values():
        sab_api("set_config", section="categories", keyword=category, dir=category, pp=0)
    sab_api("set_config", section="servers", keyword="lab", host="source", port=8119,
            ssl=0, connections=1, enable=1, optional=0, timeout=5)
    sab_api("resume")


def sab_job(download_id):
    queue = sab_api("queue", nzo_ids=download_id)["queue"]["slots"]
    history = sab_api("history", nzo_ids=download_id, start=0, limit=5)["history"]["slots"]
    archive = sab_api("history", nzo_ids=download_id, start=0, limit=5, archive=1)["history"]["slots"]
    selected = ("nzo_id", "status", "category", "name", "bytes", "completed", "loaded", "archive")
    return {name: [projection(row, selected) for row in rows if row.get("nzo_id") == download_id]
            for name, rows in (("queue", queue), ("history", history), ("archive", archive))}


def completed_job(download_id):
    rows = sab_api("history", nzo_ids=download_id, start=0, limit=5)["history"]["slots"]
    exact = [row for row in rows if row.get("nzo_id") == download_id]
    verify(not any(row["status"] == "Failed" for row in exact), "syntheticSABDownloadFailed")
    return next((row for row in exact if row["status"] == "Completed"), None)


def terminal_failed_job(download_id, title, product):
    rows = sab_api("history", nzo_ids=download_id, start=0, limit=5)["history"]["slots"]
    exact = [row for row in rows if row.get("nzo_id") == download_id]
    verify(len(exact) <= 1, "ambiguousSABHistory")
    if not exact:
        return None
    job = exact[0]
    verify(job["status"] != "Completed", "originalUnexpectedlyCompleted")
    if job["status"] != "Failed" or job.get("loaded") is not False:
        return None
    verify(job.get("name") == title and job.get("category") == CATEGORY[product]
           and job.get("archive") is False and bool(job.get("fail_message")), "SABFailureIdentityMismatch")
    snapshot = sab_job(download_id)
    if snapshot["queue"]:
        return None
    verify(len(snapshot["history"]) == 1 and snapshot["history"][0]["status"] == "Failed",
           "SABFailureReadbackMismatch")
    incomplete = safe_path(job["path"], DOWNLOADS / "incomplete")
    paths = {incomplete}
    if job.get("storage"):
        paths.add(safe_path(job["storage"], DOWNLOADS))
    outputs = [{"path": str(path), "exists": path.exists(), "media": inventory(path)} for path in sorted(paths)]
    verify(not any(output["media"] for output in outputs), "failedOriginalHasSalvageableMedia")
    return {"client": snapshot, "outputs": outputs}


def setup_fixture_dirs():
    for path in (Path("/library/sonarr"), Path("/library/radarr"), DOWNLOADS, DOWNLOADS / "complete", DOWNLOADS / "incomplete"):
        path.mkdir(exist_ok=True, parents=True)
        path.chmod(0o775)
        if path.stat().st_uid != 1000 or path.stat().st_gid != 1000:
            os.chown(path, 1000, 1000)


def candidate_for(arr, candidates, target_id, download_id):
    verify(len(candidates) == 1, "expectedSingletonManualImportPreview")
    candidate = candidates[0]
    safe_download_path(candidate["path"])
    verify(candidate.get("downloadId") in (None, download_id), "previewDownloadIDMismatch")
    verify(not candidate["rejections"], "previewRejectedSyntheticFile")
    if arr.product == "sonarr":
        verify([episode["id"] for episode in candidate["episodes"]] == [target_id], "previewEpisodeMismatch")
    else:
        verify(candidate["movie"]["id"] == target_id, "previewMovieMismatch")
    return candidate


def original_import(arr, media, report):
    name = "episode1" if arr.product == "sonarr" else "movie1"
    target_id = media["id"]
    start = len(search_events())
    command, grabbed = arr.grab(name, target_id, "original")
    arr.search_scope(start, name)
    download_id = grabbed["downloadId"]
    job = poll(lambda: completed_job(download_id), "SABCompletionTimeout", timeout=150)
    output = safe_download_path(job["storage"])
    before = {"source": inventory(output), "library": inventory(arr.library), "client": sab_job(download_id)}
    verify(len(before["source"]) == 1 and arr.file_link(target_id) == 0, "unexpectedImportStartingState")
    report["before"] = before
    report["originalGrab"] = projection(grabbed, SOURCE_KEYS)
    report["searchCommand"] = command
    arr.refresh()

    query = {"downloadId": download_id, "filterExistingFiles": "false"}
    candidates = arr.read("manualimport", **query)
    selected = candidate_for(arr, candidates, target_id, download_id)
    verify(safe_download_path(selected["path"]).is_relative_to(output), "candidateOutsideDownloadOutput")
    file_fields = ("path", "quality", "languages", "releaseGroup", "indexerFlags", "releaseType")
    reprocess = {**projection(selected, file_fields), "downloadId": download_id}
    if arr.product == "sonarr":
        reprocess.update(seriesId=selected["series"]["id"], seasonNumber=selected["seasonNumber"], episodeIds=[target_id])
    else:
        reprocess["movieId"] = target_id
    status, reprocessed = arr.api("manualimport", method="POST", body=[reprocess])
    rescanned = candidate_for(arr, reprocessed, target_id, download_id)
    verify(rescanned["path"] == selected["path"], "reprocessPathChanged")
    report["preview"] = {"query": query, "status": 200, "candidateCount": len(candidates),
                         "reprocessRequest": reprocess, "reprocessStatus": status,
                         "reprocessCount": len(reprocessed)}
    report["reprocessUnchanged"] = (
        inventory(output) == before["source"] and inventory(arr.library) == before["library"]
        and sab_job(download_id) == before["client"]
    )
    verify(report["reprocessUnchanged"], "reprocessChangedIndependentState")

    file = {**projection(rescanned, file_fields), "downloadId": download_id, "folderName": selected["folderName"]}
    if arr.product == "sonarr":
        file.update(seriesId=selected["series"]["id"], episodeIds=[target_id])
    else:
        file["movieId"] = target_id
    report["operation"] = {"method": "POST", "path": "/api/v3/command", "name": "ManualImport",
                           "importMode": "copy", "files": [file]}
    report["response"] = arr.command_status("ManualImport", importMode="copy", files=[file])
    library = poll(lambda: arr.library_file(target_id), "importFileLinkMissing")
    history = poll(lambda: [row for row in arr.history(downloadId=download_id)
                            if row["eventType"] == "downloadFolderImported"
                            and row.get("episodeId" if arr.product == "sonarr" else "movieId") == target_id],
                   "importProvenanceMissing")
    source_after = inventory(output)
    library_after = inventory(arr.library)
    report["after"] = {
        "source": source_after, "library": library_after, "libraryLink": library,
        "history": [projection(row, SOURCE_KEYS) for row in history], "client": sab_job(download_id),
    }
    report["sourcePreserved"] = source_after == before["source"]
    verify(any(item["sha256"] == before["source"][0]["sha256"] for item in library_after),
           "libraryFileContentMismatch")


def delete_queue(arr, row, flags, lose_response):
    path = f"queue/{row['id']}"
    if not lose_response:
        status, _ = arr.api(path, method="DELETE", query=flags)
        return {"httpStatus": status}

    # Dispatch the request once using a fresh transport and discard its response.
    # Readback below is the only source of evidence for downstream effects.
    connection = http.client.HTTPConnection(arr.product, arr.port, timeout=5)
    try:
        connection.request("DELETE", "/api/v3/" + path + "?" + urlencode(flags), headers=arr.headers)
    finally:
        connection.close()
    return {"transport": "closedAfterSend", "acknowledgement": "unobserved"}


def new_failure_records(arr, media, original, before_blocklist_ids, before_history_ids):
    target_id = media["id"]
    download_id, title = original["downloadId"], original["sourceTitle"]
    history = [row for row in arr.history(downloadId=download_id) if row["eventType"] == "downloadFailed"
               and row["id"] not in before_history_ids and row.get("downloadId") == download_id
               and row.get(MEDIA_FIELD[arr.product]) == target_id and row["sourceTitle"] == title]
    scope = ({"seriesId": media["seriesId"], "episodeIds": [target_id]} if arr.product == "sonarr"
             else {"movieId": target_id})
    blocklist = [row for row in arr.blocklist(media)
                 if row["id"] not in before_blocklist_ids
                 and all(row.get(key) == value for key, value in scope.items())
                 and row["sourceTitle"] == title and row["protocol"] == "usenet"
                 and row["indexer"] == INDEXER_NAME and row["quality"] == original["quality"]]
    verify(len(history) <= 1 and len(blocklist) <= 1, "rejectProvenanceAmbiguous")
    if history and blocklist:
        return {"failureHistory": [projection(row, SOURCE_KEYS) for row in history],
                "blocklist": [projection(row, BLOCKLIST_KEYS) for row in blocklist]}
    return None


def terminal_replacement(arr, media, guard, report):
    target = "episode1" if arr.product == "sonarr" else "movie1"
    target_id = media["id"]
    client = next((client for client in arr.read("downloadclient") if client["name"] == CLIENT_NAME), {})
    verify(client.get("enable") is True and client.get("protocol") == "usenet"
           and client.get("removeFailedDownloads") is False and client.get("removeCompletedDownloads") is False,
           "unexpectedClientRetentionSettings")
    report["downloadClient"] = projection(client, ("name", "protocol", "enable", "removeFailedDownloads",
                                                    "removeCompletedDownloads"))
    original_title = releases(target, "terminal-original")
    before_blocklist_ids = {row["id"] for row in arr.blocklist(media)}
    before_history_ids = {row["id"] for row in arr.history(**{HISTORY_TARGET[arr.product]: target_id})}
    verify(arr.file_link(target_id) == arr.file_link(guard["id"]) == 0 and not inventory(arr.library),
           "unexpectedReplacementStartingState")
    report["before"] = {"blocklistIDs": sorted(before_blocklist_ids), "historyIDs": sorted(before_history_ids),
                        "targetFileID": 0, "guardFileID": 0, "library": []}
    call_json(SOURCE, f"/lab/feed/{target}/terminal-original", method="POST")
    start = len(search_events())
    command, original = arr.grab(target, target_id, "terminal-original")
    report["original"] = {
        "command": command, "searches": arr.search_scope(start, target),
        "grab": projection(original, SOURCE_KEYS),
    }
    download_id = original["downloadId"]

    def read_terminal():
        return terminal_failed_job(download_id, original_title, arr.product)

    terminal = poll(read_terminal, "terminalSABFailureMissing", timeout=150)
    replies = get_json(SOURCE, f"/lab/missing-articles/{target}")
    verify(bool(replies) and all(reply["responseCode"] == 430 for reply in replies),
           "expectedMissingArticleEvidence")
    report["original"].update(terminal=terminal, nntpReplies=replies)

    search_start = len(search_events())
    arr.refresh()

    def native_failure():
        if arr.queue(download_id):
            return None
        return new_failure_records(arr, media, original, before_blocklist_ids, before_history_ids)

    failure = poll(native_failure, "arrNativeFailureReadbackMissing")
    verify(len(search_events()) == search_start and arr.file_link(target_id) == 0,
           "unexpectedWorkAfterNativeFailure")
    report["nativeFailure"] = {**failure, "queue": [], "operation": "arrDownloadMonitoring",
                               "automaticRedownloadDisabled": True}

    report["settings"]["replacementImport"] = arr.settings(True, automatic_redownload=False)
    call_json(SOURCE, f"/lab/feed/{target}/replacement", method="POST")
    verify(len(search_events()) == search_start and arr.file_link(target_id) == 0,
           "unexpectedWorkBeforeExplicitSearch")
    search_command, new_grab = arr.grab(target, target_id, "replacement")
    new_id = new_grab["downloadId"]
    verify(new_id != download_id, "replacementReusedOriginalJob")
    replacement = report["replacement"] = {
        "command": search_command, "searches": arr.search_scope(search_start, target),
        "grab": projection(new_grab, SOURCE_KEYS),
    }
    job = poll(lambda: completed_job(new_id), "replacementSABCompletionTimeout", timeout=150)
    verify(job.get("category") == CATEGORY[arr.product] and job.get("name") == new_grab["sourceTitle"],
           "replacementSABIdentityMismatch")
    replacement["client"] = sab_job(new_id)
    arr.refresh()
    library = poll(lambda: arr.library_file(target_id), "replacementLibraryFileMissing")
    path = linked_library_path(library["path"], arr.library)
    expected_media = get_json(SOURCE, "/lab/media")
    observed_media = inventory(path)
    verify(path.is_file() and len(observed_media) == 1
           and projection(observed_media[0], ("bytes", "sha256")) == expected_media
           and library["size"] == expected_media["bytes"], "replacementFileContentMismatch")

    def imported():
        rows = [row for row in arr.history(downloadId=new_id) if row["eventType"] == "downloadFolderImported"
                and row.get("downloadId") == new_id and row.get(MEDIA_FIELD[arr.product]) == target_id]
        verify(len(rows) <= 1, "replacementProvenanceAmbiguous")
        if not rows:
            return None
        row = rows[0]
        # Arr serialises dictionary keys using the API's camel-case policy.
        file_id = next((value for key, value in row["data"].items() if key.casefold() == "fileid"), None)
        verify(str(library["id"]) == file_id, "replacementProvenanceFileMismatch")
        return {**projection(row, SOURCE_KEYS), "fileId": file_id}

    provenance = poll(imported, "replacementProvenanceMissing")
    verify(not any(row["eventType"] == "downloadFolderImported" for row in arr.history(downloadId=download_id)),
           "failedOriginalImported")
    original_after = read_terminal()
    library_after = inventory(arr.library)
    replacement.update(library=library, history=provenance, libraryInventory=library_after,
                       fixtureMedia=expected_media, guardFileID=arr.file_link(guard["id"]),
                       originalAfter=original_after, originalRetained=original_after == terminal)
    verify(replacement["originalRetained"], "failedOriginalDispositionChanged")
    verify(replacement["guardFileID"] == 0 and library_after == observed_media, "unrelatedTargetFileChanged")


def reject_replace(arr, media, guard, report, remove, lose_response):
    target = "episode1" if arr.product == "sonarr" else "movie1"
    target_id = media["id"]
    sab_api("pause")
    start = len(search_events())
    command, original = arr.grab(target, target_id, "original")
    arr.search_scope(start, target)
    download_id = original["downloadId"]
    arr.refresh()
    rows = poll(lambda: arr.queue(download_id), "originalQueueRowMissing")
    expected_scope_field = "episodeId" if arr.product == "sonarr" else "movieId"
    verify(len(rows) == 1 and rows[0].get(expected_scope_field) == target_id
           and rows[0]["protocol"] == "usenet", "originalQueueScopeMismatch")
    effects = {"client": sab_job(download_id), "blocklist": arr.blocklist(media),
               "history": arr.history(downloadId=download_id), "library": inventory(arr.library)}
    verify(len(effects["client"]["queue"]) == 1 and arr.file_link(target_id) == 0,
           "unexpectedRejectStartingState")
    before_ids = {item["id"] for item in effects["blocklist"]}
    before_history_ids = {item["id"] for item in effects["history"]}
    verify(not any(row["eventType"] == "downloadFailed" for row in effects["history"]),
           "originalAlreadyFailedInArr")
    report["before"] = {
        "originalGrab": projection(original, SOURCE_KEYS), "searchCommand": command,
        "queue": [projection(item, SOURCE_KEYS) for item in rows], "client": effects["client"],
        "existingBlocklistRows": sorted(before_ids), "existingHistoryRows": sorted(before_history_ids),
        "library": effects["library"],
    }
    report["context"] = "SAB queue branch with a paused synthetic download"
    report["automaticRemovalQualified"] = False
    flags = {"removeFromClient": str(remove).lower(), "blocklist": "true",
             "skipRedownload": "true", "changeCategory": "false"}
    report["operation"] = {"method": "DELETE", "path": f"/api/v3/queue/{rows[0]['id']}", "query": flags}
    search_start = len(search_events())
    report["response"] = delete_queue(arr, rows[0], flags, lose_response)

    failure = poll(lambda: new_failure_records(arr, media, original, before_ids, before_history_ids),
                   "rejectFailureReadbackMissing")
    reject_measured = time.monotonic()
    time.sleep(15)
    unrelated_searches = search_events()[search_start:]
    report["afterReject"] = {
        **failure,
        "client": sab_job(download_id), "library": inventory(arr.library),
        "arrQueue": [projection(row, SOURCE_KEYS) for row in arr.queue(download_id)],
        "searchObservationSeconds": round(time.monotonic() - reject_measured, 3),
        "searchRequests": unrelated_searches,
    }
    verify(not unrelated_searches, "unexpectedSearchDuringSkipObservation")
    observed_queue = report["afterReject"]["client"]["queue"]
    verify(len(observed_queue) == (0 if remove else 1), "unexpectedSABQueueDisposition")
    verify(not report["afterReject"]["arrQueue"], "rejectedArrQueueStillVisible")
    if not remove:
        # The pre-existing paused job would otherwise be downloaded when resuming
        # SAB for the separately evidenced explicit replacement.
        sab_api("queue", name="pause", value=download_id)
        verify(bool(sab_job(download_id)["queue"]), "preservedSABJobMissingBeforeReplacement")

    call_json(SOURCE, f"/lab/feed/{target}/replacement", method="POST")
    new_search_start = len(search_events())
    search_command, new_grab = arr.grab(target, target_id, "replacement")
    verify(new_grab["downloadId"] != download_id, "replacementReusedOriginalJob")
    searches = arr.search_scope(new_search_start, target)
    report["replacement"] = {"command": search_command, "searches": searches,
                             "grab": projection(new_grab, SOURCE_KEYS)}
    if not remove:
        # Keep the preserved original in its observed state. Without a genuine
        # terminal original, resuming both jobs could race their automatic imports.
        report["replacement"]["importStatus"] = "awaitingTerminalFixture"
        return
    arr.settings(True)
    sab_api("resume")
    job = poll(lambda: completed_job(new_grab["downloadId"]), "replacementSABCompletionTimeout", timeout=150)
    arr.refresh()
    library = poll(lambda: arr.library_file(target_id), "replacementLibraryFileMissing")
    provenance = poll(lambda: [row for row in arr.history(downloadId=new_grab["downloadId"])
                               if row["eventType"] == "downloadFolderImported"
                               and row.get(expected_scope_field) == target_id], "replacementProvenanceMissing")
    verify(arr.file_link(guard["id"]) == 0, "unrelatedTargetFileChanged")
    report["replacement"].update(
        library=library, history=[projection(row, SOURCE_KEYS) for row in provenance],
        client=sab_job(job["nzo_id"]), libraryInventory=inventory(arr.library),
    )


def run_scenarios(report, scenario):
    product, case = scenario.split("/", 1)
    runs = report.setdefault("scenarios", {})
    result = runs[scenario] = {
        "status": "running", "evidenceClass": "labObservation", "protocol": "usenet",
        "downloader": "SABnzbd", "fixtureSeed": SEED,
    }
    started = time.monotonic()
    try:
        result["stage"] = "sourceStartup"
        wait_for(lambda: get_json(SOURCE, "/health"))
        setup_fixture_dirs()
        result["stage"] = "sabSetup"
        configure_sab()
        arr = Arr(product)
        result["stage"] = "arrSetup"
        terminal_case = case == "replace-terminal"
        profile = arr.setup(automatic_redownload=not terminal_case)
        result["settings"] = {
            "profile": PROFILE_NAME, "profileId": profile["id"], "cutoff": profile["cutoff"],
            "root": "/library", "category": CATEGORY[product], "completedHandling": False,
            "removeCompletedDownloads": False, "removeFailedDownloads": False, "rss": False,
            "automaticRedownloadFailed": not terminal_case,
        }
        result["stage"] = "mediaSetup"
        targets = arr.media(profile["id"])
        result["stage"] = "exercise"
        if case == "import":
            original_import(arr, targets[0], result)
        elif terminal_case:
            terminal_replacement(arr, targets[0], targets[1], result)
        else:
            reject_replace(arr, targets[0], targets[1], result, remove=case == "reject-remove",
                           lose_response=case == "reject-lost-response")
        if case == "import" or terminal_case:
            result.update(status="observed", stage="complete", qualification="awaitingIndependentReview")
        else:
            result.update(status="observed", stage="complete", qualification="terminalOriginalUnproven")
    except LabFailure as failure:
        result.update(status="failed", reason=str(failure))
    except (OSError, RuntimeError, ValueError, KeyError, TypeError, AttributeError) as failure:
        result.update(status="failed", reason=type(failure).__name__)
    finally:
        try:
            result["fixtureMetadata"] = get_json(SOURCE, "/lab/metadata")
        except (OSError, ValueError):
            result["fixtureMetadata"] = {"status": "unavailable"}
        result["elapsedSeconds"] = round(time.monotonic() - started, 3)
    return result["status"] == "observed"
