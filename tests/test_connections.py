"""Tests that credentials never leak into API response data."""

import asyncio

from rescuarr.clients import check_all


def test_unconfigured_services(monkeypatch):
    for key in ("SONARR_URL", "SONARR_API_KEY", "RADARR_URL", "RADARR_API_KEY",
                "SABNZBD_URL", "SABNZBD_API_KEY"):
        monkeypatch.delenv(key, raising=False)
    services = asyncio.run(check_all())
    assert [service["service"] for service in services] == ["Sonarr", "Radarr", "SABnzbd"]
    assert all(service["status"] == "not_configured" for service in services)
    assert all(service["reachable"] is False for service in services)
