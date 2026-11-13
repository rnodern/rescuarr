# Roadmap

## M0 — Bootstrap (current)

- [x] Project name, purpose, README, and initial Git history
- [x] Read-only service connectivity API
- [x] Local Docker build / Compose configuration
- [ ] Publish GitHub repository
- [ ] Add basic CI workflow and initial screenshots

## M1 — Observe

- [ ] Poll Sonarr/Radarr queues and SABnzbd history
- [ ] Correlate failed imports to original grab IDs
- [ ] Capture actionable error reasons and recurrence
- [ ] Display candidates in a simple dashboard
- [ ] Add mocked integration tests with sample payloads (no real media details)

## M2 — Assisted recovery

- [ ] Identify valid media with ffprobe and optional read-only filesystem access
- [ ] Safe manual import with before/after verification
- [ ] Approve/reject actions with an audit trail
- [ ] Blocklist/retry specific releases via Sonarr/Radarr APIs

## M3 — Autonomous rules

- [ ] Per-error policies, retry limits, grace periods, and idempotency
- [ ] Configuration-fault circuit breaker
- [ ] Notifications and daily recovery summary
- [ ] Optional metrics and dashboard filtering

## M4 — Release

- [ ] Authentication, role-based permissions, secrets handling
- [ ] CI, static analysis, integration tests, and signed container image
- [ ] Versioned documentation, release notes, licence decision
