"""Drive the packaged owner console against the disposable Windows Sonarr lab."""

import json
import os
import re
import sqlite3
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from contextlib import closing
from html.parser import HTMLParser
from http.cookiejar import CookieJar

from probe import NoRedirect, SAB_VERSION, call_json
from scenarios import LabFailure, inventory, poll, projection, sab_job, search_events, verify

ORIGIN = "http://127.0.0.1:18987"
REVIEW = "/manual-review"
PASSWORD = "Disposable Rescuarr owner password"
CONNECTION_CHECK = re.compile(r"/connections/([A-Za-z0-9_-]{1,128})/check")


class PostForms(HTMLParser):
    def __init__(self, body):
        super().__init__(convert_charrefs=True)
        self.forms = []
        self.current = None
        self.feed(body)

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if tag == "form" and attrs.get("method", "").lower() == "post":
            self.current = {}
            self.forms.append((attrs.get("action"), self.current))
        elif self.current is not None and tag == "input" and attrs.get("type") == "hidden":
            name = attrs.get("name")
            if name:
                self.current.setdefault(name, []).append(attrs.get("value", ""))

    def handle_endtag(self, tag):
        if tag == "form":
            self.current = None

    def form(self, action, **matches):
        matches = [fields for path, fields in self.forms
                   if path == action and all(fields.get(key) == [value] for key, value in matches.items())]
        if not matches:
            return None
        verify(len(matches) == 1 and all(len(values) == 1 for values in matches[0].values())
               and bool(matches[0].get("csrf", [""])[0]), "ambiguousManualBrowserForm")
        return {key: values[0] for key, values in matches[0].items()}


class OwnerBrowser:
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect(),
                                                urllib.request.HTTPCookieProcessor(CookieJar()))

    def get(self, path):
        try:
            with self.opener.open(ORIGIN + path, timeout=35) as response:
                content = response.read(512_001)
            verify(len(content) <= 512_000, "manualBrowserResponseTooLarge")
            return content.decode("utf-8")
        except urllib.error.HTTPError as error:
            with error:
                raise LabFailure(f"manualBrowserGET:{path}:HTTP{error.code}") from None

    def post(self, path, expected, fields):
        req = urllib.request.Request(ORIGIN + path, data=urllib.parse.urlencode(fields).encode("ascii"),
                                     headers={"Origin": ORIGIN}, method="POST")
        try:
            with self.opener.open(req, timeout=35):
                verify(False, "manualBrowserPOSTDidNotRedirect")
        except urllib.error.HTTPError as error:
            with error:
                verify(error.code == 303 and error.headers.get("Location") == expected,
                       f"manualBrowserPOST:{path}:HTTP{error.code}")

    def form(self, path, action, **matches):
        form = PostForms(self.get(path)).form(action, **matches)
        verify(form is not None, f"manualBrowserFormMissing:{path}")
        return form

    def setup(self, directory):
        fields = self.form("/setup", "/setup")
        fields.update(token=(directory / "setup-token").read_text(encoding="ascii").strip(), password=PASSWORD)
        self.post("/setup", "/login", fields)
        self.login()

    def login(self):
        fields = self.form("/login", "/login")
        self.post("/login", "/", {**fields, "password": PASSWORD})

    def connect(self, arr):
        fields = self.form("/connections", "/connections")
        self.post("/connections", "/connections", {**fields, "product": "sonarr", "url": arr.origin,
                  "api_key": arr.headers["X-Api-Key"], "name": "Disposable Sonarr"})
        forms = PostForms(self.get("/connections"))
        checks = [path for path, _ in forms.forms if path and CONNECTION_CHECK.fullmatch(path)]
        verify(len(checks) == 1, "manualBrowserConnectionCheckFormMissing")
        self.post(checks[0], "/connections", forms.form(checks[0]))
        status_page = self.get("/connections")
        if ">Available<" not in status_page or ">Complete<" not in status_page:
            labels = {"API key rejected or unavailable": "access", "Service unavailable": "unavailable",
                      "Unsupported response": "response", "Queue read incomplete": "partial",
                      "Version changed": "version", "Awaiting first check": "awaitingCheck"}
            outcome = next((label for message, label in labels.items() if message in status_page), "unknown")
            raise LabFailure(f"manualBrowserConnectionCheckNotReady:{outcome}")
        return CONNECTION_CHECK.fullmatch(checks[0])[1]

    def manual_mode(self):
        verify('href="/manual-review"' not in self.get("/"), "manualNavPresentInObserve")
        fields = self.form("/", REVIEW + "/mode", mode="manual")
        self.post(REVIEW + "/mode", REVIEW, fields)
        verify('href="/manual-review"' in self.get("/"), "manualNavMissing")

    def review(self, title, folder):
        def checked():
            page = self.get(REVIEW)
            verify("<h1>Manual Review</h1>" in page, "manualBrowserReviewPageMissing")
            forms = PostForms(page)
            decline = forms.form(REVIEW, decision="decline")
            approval = forms.form(REVIEW, decision="approve")
            if not decline or not approval:
                return None
            verify(title in page and folder in page and "found no eligible files" in page
                   and "Already in library" in page and "Still missing" in page
                   and "Remove this download from the Sonarr queue." in page
                   and "Blocklist this release and keep the download client job." in page
                   and re.search(r"Search only for S01E01[^<]*, if still missing", page) is not None
                   and "Search only for S01E02" not in page, "manualBrowserReviewEvidenceIncomplete")
            verify(bool(decline.get("token")) and bool(approval.get("token")),
                   "manualBrowserDecisionTokenMissing")
            return decline, approval

        return poll(checked, "manualBrowserReviewDeadline", timeout=60)

    def activity(self, marker):
        return poll(lambda: marker in self.get("/activity"), "manualBrowserActivityDeadline", timeout=45)


class PackagedManualLab:
    def __init__(self, executable, root, stack, launch, await_ready, stop, on_stage, source, source_sha):
        self.executable = executable
        self.root = root
        self.stack = stack
        self.launch = launch
        self.await_ready = await_ready
        self.stop = stop
        self.on_stage = on_stage
        self.source = source
        self.environment = {key: value for key, value in os.environ.items() if not key.startswith("RESCUARR_")}
        self.version = self.command("version").strip()
        verify(re.fullmatch(rf"rescuarr [\w.\-]+ \({source_sha}\)", self.version), "manualBrowserPackagedVersionMismatch")

    def command(self, *args):
        result = subprocess.run([self.executable, *args], cwd=self.root, env=self.environment,
                                capture_output=True, text=True, timeout=40, check=False)
        if result.returncode != 0:
            known = {"run Check queue in the console first": "selectConnection",
                     "observed Sonarr version differs": "savedVersion",
                     "current version differs": "currentVersion",
                     "selected Sonarr connection's API key": "localCredential",
                     "one enabled SABnzbd client": "upstreamProfile",
                     "data directory is already in use": "localLock"}
            detail = next((code for message, code in known.items() if message in result.stderr), "unknown")
            raise LabFailure(f"manualBrowserCLI:{args[0]}:exit{result.returncode}:{detail}")
        return result.stdout

    def start(self, directory, settings=None):
        proc = self.launch(self.stack, [self.executable, "serve", "--data-dir", directory, "--listen",
                                       urllib.parse.urlsplit(ORIGIN).netloc],
                           self.root, env={**self.environment, **(settings or {})})
        browser = OwnerBrowser()

        def ready():
            with browser.opener.open(ORIGIN + "/readyz", timeout=2) as response:
                return response.status == 200

        self.await_ready(proc, ready, "manualBrowser")
        return proc, browser

    def setup_manual_review(self, arr, directory):
        proc, browser = self.start(directory)
        browser.setup(directory)
        connection_id = browser.connect(arr)
        self.stop(proc)
        profile = self.command("check-empty-pack", "--data-dir", directory, "--sonarr-connection", connection_id)
        found = re.search(r"^Profile SHA-256: ([a-f0-9]{64})$", profile, flags=re.MULTILINE)
        verify(found is not None, "manualBrowserProfileStampMissing")
        settings = {
            "RESCUARR_MANUAL_EMPTY_PACK_CONNECTION": connection_id,
            "RESCUARR_MANUAL_EMPTY_PACK_PROFILE": found[1],
            "RESCUARR_MANUAL_EMPTY_PACK_SAB_VERSION": SAB_VERSION,
        }
        proc, browser = self.start(directory, settings)
        browser.login()
        browser.manual_mode()
        return proc, browser, settings

    def run(self, arr, download_id, title, folder, search_id, report):
        def queue_state():
            keys = ("id", "seriesId", "episodeId", "status", "trackedDownloadState", "statusMessages")
            return [projection(row, keys) for row in sorted(arr.queue(download_id), key=lambda row: row["id"])]

        self.on_stage("manualBrowserDecline")
        initial_queue = queue_state()
        initial_sab = sab_job(download_id)
        initial_searches = search_events()
        initial_library = inventory(arr.library, all_files=True)

        def unchanged(reason):
            verify(queue_state() == initial_queue and sab_job(download_id) == initial_sab
                   and search_events() == initial_searches and inventory(arr.library, all_files=True) == initial_library
                   and folder.is_dir() and not inventory(folder, all_files=True), reason)

        proc, browser, _ = self.setup_manual_review(arr, self.root / "rescuarr-decline")
        decline, _ = browser.review(title, folder.name)
        browser.post(REVIEW, REVIEW, decline)
        browser.activity("Manual recovery declined")
        verify(PostForms(browser.get(REVIEW)).form(REVIEW, decision="approve") is None,
               "manualBrowserDeclinedCaseOfferedAgain")
        self.stop(proc)
        unchanged("manualBrowserDeclineChangedUpstream")

        self.on_stage("manualBrowserProfile")
        data = self.root / "rescuarr-approval"
        proc, browser, settings = self.setup_manual_review(arr, data)
        decline, _ = browser.review(title, folder.name)
        reloaded, _ = browser.review(title, folder.name)
        verify(reloaded["token"] == decline["token"], "manualBrowserCaseChangedOnReload")
        self.stop(proc)
        proc, browser = self.start(data, settings)
        browser.login()
        restored, approval = browser.review(title, folder.name)
        verify(restored["token"] == decline["token"], "manualBrowserCaseDidNotSurviveRestart")
        time.sleep(6)
        unchanged("manualBrowserChangedUpstreamBeforeApproval")

        self.on_stage("manualBrowserApprove")
        call_json(self.source, "/lab/feed/episode1/replacement", method="POST")
        browser.post(REVIEW, REVIEW, approval)
        browser.activity("Sonarr accepted the episode search")
        with closing(sqlite3.connect((data / "rescuarr.sqlite3").as_uri() + "?mode=ro", uri=True)) as connection:
            records = connection.execute(
                "SELECT stage, search_ids, command_id FROM empty_pack_recovery WHERE download_id = ?", (download_id,)
            ).fetchall()
        verify(len(records) == 1 and records[0][0] == "done" and json.loads(records[0][1]) == [search_id]
               and records[0][2] > 0, "manualBrowserDurableSearchMissing")
        report["manualBrowser"] = {"packagedVersion": self.version, "declineRecorded": True,
                                   "declineMadeNoChanges": True, "reviewSurvivedReload": True, "reviewSurvivedRestart": True,
                                   "noChangesBeforeApproval": True, "searchRecordedInActivity": True,
                                   "searchCommandId": records[0][2]}
        return records[0][2]
