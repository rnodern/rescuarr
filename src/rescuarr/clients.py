"""Read-only health checks for the three integrations.

No mutating API endpoints are called in this prototype.
"""

from __future__ import annotations

import asyncio
import os

import httpx


def configured_services() -> list[dict[str, str | bool]]:
    """Load credentials at request time without returning secrets to callers."""
    services = []
    for name, prefix in (("Sonarr", "SONARR"), ("Radarr", "RADARR"), ("SABnzbd", "SABNZBD")):
        url = os.getenv(f"{prefix}_URL", "").strip().rstrip("/")
        key = os.getenv(f"{prefix}_API_KEY", "").strip()
        services.append({"name": name, "prefix": prefix, "url": url, "key": key,
                         "configured": bool(url and key)})
    return services


async def check_service(client: httpx.AsyncClient, service: dict[str, str | bool]) -> dict[str, object]:
    """Return a minimal status without exposing URLs, file paths, or API keys."""
    result: dict[str, object] = {"service": service["name"],
                                 "configured": service["configured"], "reachable": False}
    if not service["configured"]:
        result["status"] = "not_configured"
        return result

    try:
        if service["prefix"] == "SABNZBD":
            response = await client.get(f"{service['url']}/api", params={
                "mode": "version", "output": "json", "apikey": service["key"]})
        else:
            response = await client.get(f"{service['url']}/api/v3/system/status",
                                        headers={"X-Api-Key": str(service["key"])})
        response.raise_for_status()
        response.json()  # Verify this really is an API response.
        result["reachable"] = True
        result["status"] = "ok"
    except (httpx.HTTPError, ValueError):
        # Avoid returning exception details: they can include URLs and API keys.
        result["status"] = "error"
    return result


async def check_all() -> list[dict[str, object]]:
    async with httpx.AsyncClient(timeout=5, follow_redirects=False) as client:
        return await asyncio.gather(*(check_service(client, service)
                                      for service in configured_services()))
