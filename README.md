# Rescuarr

**No download left behind.**

Rescuarr is a self-hosted download recovery assistant for **Sonarr**, **Radarr**, and **SABnzbd**. The Go preview lets an owner sign in, add Arr connections, check queues and review saved cases.

> **Project status** · Observe preview. Recovery actions are held pending end-to-end qualification.

## Principles

- **Recover before replacing.** Preserve usable downloads whenever possible.
- **Diagnose the cause.** Don't redownload when the real problem is permissions, paths, or configuration.
- **Avoid destructive guesses.** Never auto-import an ambiguous file or auto-delete a successful import.
- **Use the source of truth.** Manage blocklisting and replacement through Sonarr/Radarr, not solely through SABnzbd.
- **Audit every action.** Explain what happened, why, and whether it succeeded.

## Planned operating modes

| Mode | Behaviour |
| --- | --- |
| Observe | Check queues and review saved cases |
| Assist | Review and approve an eligible recovery action |
| Autonomous | Apply explicitly enabled recovery policies with retry limits and auditing |

## Run the Observe preview

Use the [native and container install instructions](docs/INSTALL.md) for a checked package. From a source checkout with Go 1.27.4, or a recent Docker Compose v2 with support for `include`, choose one of these commands.

```sh
go run ./cmd/rescuarr serve
```

```sh
docker compose up --build -d
```

Open `http://127.0.0.1:8787`. Read the private `setup-token` file from the data directory, or run `docker compose exec -T rescuarr cat /data/setup-token`. Set an owner password and sign in. In **Connections**, enter the Arr base URL and paste its API key, then select **Add connection**. Use **Check queue** to scan immediately. To stop scanning a service, use **Remove** on its connection. Past recovery activity stays available.

## Repository layout

- `cmd/rescuarr/` and `internal/` — Go application
- `deploy/` — native service examples and container build
- `test/lab/` — disposable compatibility scenarios
- `src/rescuarr/`, `tests/` and `legacy/` — historical Python connectivity prototype
- [Product and system design](docs/ARCHITECTURE.md)
- [Recovery and deterministic learning](docs/RECOVERY_DESIGN.md)
- [Service integration contracts](docs/INTEGRATIONS_DESIGN.md)
- [Web console design](docs/UI_DESIGN.md)
- [Delivery and verification plan](docs/ROADMAP.md)
- [Publishing and brand checklist](docs/NAME_AND_RELEASE.md)

## Licence

No open-source licence has been selected yet. All rights are reserved until a licence is deliberately chosen. This is not a registered trademark claim.
