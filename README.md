# Rescuarr

**No download left behind.**

Rescuarr is a planned, self-hosted download recovery assistant for **Sonarr**, **Radarr**, and **SABnzbd**. It aims to explain why completed downloads are stuck awaiting manual intervention, salvage valid media where safe, and blocklist/retry truly unusable releases.

> **Project status:** Pre-alpha bootstrap. The working API only checks connectivity. It cannot import, delete, blocklist, or retry downloads yet.

## Principles

- **Recover before replacing.** Preserve usable downloads whenever possible.
- **Diagnose the cause.** Don't redownload when the real problem is permissions, paths, or configuration.
- **Avoid destructive guesses.** Never auto-import an ambiguous file or auto-delete a successful import.
- **Use the source of truth.** Manage blocklisting and replacement through Sonarr/Radarr, not solely through SABnzbd.
- **Audit every action.** Explain what happened, why, and whether it succeeded.

## Planned operating modes

| Mode | Behaviour |
| --- | --- |
| Observe | Classify failures and suggest actions; read-only |
| Assist | Review and approve an eligible recovery action |
| Autonomous | Apply explicitly enabled recovery policies with retry limits and auditing |

## Quick start (connectivity prototype)

Requires Python 3.11+.

```bash
python -m venv .venv
source .venv/bin/activate
pip install -e '.[dev]'
cp .env.example .env
# Edit .env with your own local URLs and API keys.
set -a && source .env && set +a
uvicorn rescuarr.app:app --host 127.0.0.1 --port 8787
```

Then open `http://127.0.0.1:8787/docs` or `http://127.0.0.1:8787/api/connections`.

Alternatively, once you have edited `.env`, run `docker compose up --build -d`. The compose file publishes only on local loopback by default.

**Security:** Keep API keys private. Never commit `.env`, logs, download paths, or media content. The prototype has no user authentication; do not expose port 8787 to the internet.

## Repository layout

- `src/rescuarr/` — connectivity API bootstrap
- `tests/` — basic tests
- [Product and system design](docs/ARCHITECTURE.md)
- [Recovery and deterministic learning](docs/RECOVERY_DESIGN.md)
- [Service integration contracts](docs/INTEGRATIONS_DESIGN.md)
- [Web console design](docs/UI_DESIGN.md)
- [Delivery and verification plan](docs/ROADMAP.md)
- [Publishing and brand checklist](docs/NAME_AND_RELEASE.md)

## Licence

No open-source licence has been selected yet. All rights are reserved until a licence is deliberately chosen. This is not a registered trademark claim.
