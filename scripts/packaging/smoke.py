"""Use the packaged service and inspect its persisted SQLite installation ID."""

from contextlib import closing
from html.parser import HTMLParser
from http.cookiejar import CookieJar
import os
import sqlite3
import time
import urllib.error
import urllib.parse
import urllib.request


def wait_for_http(base_url, running):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    deadline = time.monotonic() + 60
    last_error = None
    while time.monotonic() < deadline and running():
        try:
            for route in ("/healthz", "/readyz"):
                with opener.open(base_url + route, timeout=2) as response:
                    if response.status != 200:
                        raise RuntimeError(f"{route} returned {response.status}")
            return
        except (OSError, urllib.error.URLError, RuntimeError) as error:
            last_error = error
            time.sleep(0.2)
    if last_error is not None:
        raise RuntimeError(f"Packaged process did not become ready: {last_error}")
    raise RuntimeError("Packaged process stopped before the first health probe")


def check_setup_page(base_url):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open(base_url + "/", timeout=5) as response:
        if response.geturl() != base_url + "/setup" or b"Set owner password" not in response.read(64 * 1024):
            raise RuntimeError("Packaged browser did not open the first-run setup page")


def installation_state(database):
    with closing(sqlite3.connect(database.as_uri() + "?mode=ro", uri=True)) as connection:
        result = connection.execute(
            "SELECT installation_id, mode, reconcile_required FROM app_state WHERE singleton = 1"
        ).fetchone()
    if not result or not result[0]:
        raise RuntimeError("SQLite has no durable installation ID")
    return result


def check_setup_token(data):
    token = data / "setup-token"
    metadata = token.stat()
    if not token.is_file() or metadata.st_size == 0:
        raise RuntimeError("First startup failed to create a setup token")
    if os.name != "nt" and metadata.st_mode & 0o077:
        raise RuntimeError("Setup token is accessible to group or other users")

class OwnerForm(HTMLParser):
    def __init__(self, html, action):
        super().__init__(convert_charrefs=True)
        self.action = action
        self.current = False
        self.form_count = 0
        self.tokens = []
        self.feed(html.decode("utf-8"))

    def handle_starttag(self, tag, attributes):
        attrs = dict(attributes)
        if tag == "form":
            self.current = attrs.get("action") == self.action and attrs.get("method", "").lower() == "post"
            self.form_count += int(self.current)
        elif self.current and tag == "input" and attrs.get("name") == "csrf" and attrs.get("type") == "hidden":
            self.tokens.append(attrs.get("value", ""))

    def handle_endtag(self, tag):
        if tag == "form":
            self.current = False

    def csrf(self):
        if self.form_count != 1 or len(self.tokens) != 1 or not self.tokens[0]:
            raise RuntimeError("Owner page did not include a unique form token")
        return self.tokens[0]


def check_owner_workflow(base_url, data):
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(CookieJar()))

    def get(path, marker):
        with opener.open(base_url + path, timeout=5) as response:
            content = response.read(65537)
            if response.geturl() != base_url + path or response.status != 200 or len(content) > 65536 or marker not in content:
                raise RuntimeError(f"Owner workflow page is invalid ({path})")
            return content

    def post(path, expected, values):
        request = urllib.request.Request(base_url + path, data=urllib.parse.urlencode(values).encode("ascii"),
                                         headers={"Origin": base_url}, method="POST")
        with opener.open(request, timeout=10) as response:
            if response.geturl() != base_url + expected or response.status != 200:
                raise RuntimeError(f"Owner workflow did not redirect as expected ({path})")

    def anonymous_cases():
        try:
            opener.open(base_url + "/cases", timeout=5).close()
        except urllib.error.HTTPError as error:
            with error:
                if error.code == 401:
                    return
        raise RuntimeError("Cases are accessible without an owner session")

    anonymous_cases()
    password = "Rescuarr synthetic packaging password"
    setup = OwnerForm(get("/setup", b"Set owner password"), "/setup")
    token = (data / "setup-token").read_text(encoding="ascii").strip()
    post("/setup", "/login", {"csrf": setup.csrf(), "token": token, "password": password})
    login = OwnerForm(get("/login", b"Sign in"), "/login")
    post("/login", "/", {"csrf": login.csrf(), "password": password})
    get("/cases", b"<h1>Cases</h1>")
    get("/connections", b"<h1>Connections</h1>")
    for suffix, media in (("css", "text/css"), ("js", "javascript")):
        path = "/assets/console." + suffix
        with opener.open(base_url + path, timeout=5) as response:
            if response.geturl() != base_url + path or media not in response.headers.get("Content-Type", ""):
                raise RuntimeError("The authenticated console asset has an invalid media type")
    overview = OwnerForm(get("/", b"<h1>Overview</h1>"), "/logout")
    post("/logout", "/login", {"csrf": overview.csrf()})
    anonymous_cases()
