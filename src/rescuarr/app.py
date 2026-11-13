"""Read-only Rescuarr API bootstrap."""

from fastapi import FastAPI
from rescuarr import __version__
from rescuarr.clients import check_all

app = FastAPI(title="Rescuarr", version=__version__,
              description="No download left behind. Pre-alpha, read-only connectivity checks.")


@app.get("/health")
async def health() -> dict[str, str]:
    return {"status": "ok", "version": __version__}


@app.get("/api/connections")
async def connections() -> dict[str, object]:
    return {"services": await check_all(), "mode": "observe_only"}
