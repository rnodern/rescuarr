"""Use the packaged service and inspect its persisted SQLite installation ID."""

from contextlib import closing
import os
import sqlite3
import time
import urllib.error
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
    raise RuntimeError(f"Packaged process did not become ready: {last_error}")


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
