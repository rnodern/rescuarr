# Architecture and safety

## Scope

Rescuarr is an orchestrator, not a replacement for Sonarr, Radarr, or SABnzbd.

1. Poll the Sonarr/Radarr activity queues and their warning/status details.
2. Correlate records with download-client queue/history, download IDs, and grab history.
3. Classify failure causes: naming/identification, archive/extraction, quality/duplicate,
   permissions/path, missing media, corrupt/incomplete, and ambiguous multi-file releases.
4. Assess whether the original download can be imported safely.
5. Ask for approval or act within explicitly configured policies.
6. Verify the outcome in Sonarr/Radarr before cleanup.
7. Record decisions, source error text, action attempts, and outcomes in a local audit log.

## Planned safeguards

- Observe-only by default. Mutating operations are **not implemented** yet.
- No auto-force-import for unknown title/episode identity or ambiguous season packs.
- No filesystem permission changes without explicit operator approval.
- No blind deletion of download folders; let Sonarr/Radarr own the rejected-release lifecycle.
- Stop after a bounded number of replacement attempts and avoid repeat download loops.
- Per-release deduplication and locks to avoid concurrent worker races.
- Never emit API keys, complete download paths, or download-client authentication parameters in logs.
- Do not expose the unauthenticated prototype outside localhost.

## Phased components

- **API adapter:** HTTP clients for Sonarr/Radarr and SABnzbd.
- **Correlator:** Match queue records to download-client jobs and grab history.
- **Classifier:** Deterministic rules for known failure types.
- **Policy engine:** Assess safety and require approvals when necessary.
- **Executor:** Future import, blocklist, reject, and retry commands.
- **Audit store:** SQLite; migrations and retention policy to be added.
- **Dashboard:** Future web UI showing action candidates and audit history.

## Development constraints

No production credentials or sensitive paths are included in this repository.
Read-only status checks are the only implemented integration behaviour.
