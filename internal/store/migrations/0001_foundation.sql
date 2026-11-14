CREATE TABLE app_state (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    installation_id TEXT NOT NULL,
    mode TEXT NOT NULL CHECK (mode IN ('observe', 'assist', 'autonomous')),
    reconcile_required INTEGER NOT NULL CHECK (reconcile_required IN (0, 1)),
    health_probe INTEGER NOT NULL DEFAULT 0
) STRICT;

INSERT INTO app_state (singleton, installation_id, mode, reconcile_required)
VALUES (1, lower(hex(randomblob(16))), 'observe', 0);

CREATE TABLE audit_events (
    sequence INTEGER PRIMARY KEY AUTOINCREMENT,
    id TEXT NOT NULL UNIQUE,
    kind TEXT NOT NULL,
    actor TEXT NOT NULL,
    scope_kind TEXT NOT NULL,
    scope_key TEXT NOT NULL,
    instance_id TEXT NOT NULL,
    instance_epoch INTEGER NOT NULL,
    case_id TEXT NOT NULL,
    incident_id TEXT NOT NULL,
    intent_id TEXT NOT NULL,
    reason TEXT NOT NULL,
    evidence TEXT NOT NULL CHECK (json_valid(evidence)),
    occurred_at TEXT NOT NULL
) STRICT;

CREATE TABLE notification_outbox (
    id TEXT PRIMARY KEY,
    event_id TEXT NOT NULL REFERENCES audit_events(id),
    route_id TEXT NOT NULL,
    route_revision INTEGER NOT NULL,
    dedupe_key TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'sending', 'accepted', 'suppressed', 'delayed', 'failed', 'uncertain')),
    scheduled_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    reason TEXT NOT NULL,
    UNIQUE (event_id, route_id),
    UNIQUE (route_id, route_revision, dedupe_key)
) STRICT;

CREATE INDEX notification_outbox_schedule ON notification_outbox (state, scheduled_at, id);
