CREATE TABLE prowl_routes (
    id TEXT PRIMARY KEY,
    revision INTEGER NOT NULL CHECK (revision > 0),
    key_ref TEXT NOT NULL,
    enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    paused INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1)),
    mask INTEGER NOT NULL CHECK (mask BETWEEN 0 AND 31),
    timezone TEXT NOT NULL,
    quiet_start INTEGER NOT NULL CHECK (quiet_start BETWEEN 0 AND 1439),
    quiet_end INTEGER NOT NULL CHECK (quiet_end BETWEEN 0 AND 1439),
    digest INTEGER NOT NULL CHECK (digest IN (0, 1)),
    not_before TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE prowl_deliveries (
    outbox_id TEXT PRIMARY KEY REFERENCES notification_outbox(id) ON DELETE CASCADE,
    route_id TEXT NOT NULL REFERENCES prowl_routes(id),
    category TEXT NOT NULL CHECK (category IN ('attention', 'recovery', 'incident', 'resolution', 'pause', 'test', 'digest')),
    collected INTEGER NOT NULL DEFAULT 1 CHECK (collected > 0),
    lease_until TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE TABLE prowl_attempts (
    outbox_id TEXT NOT NULL REFERENCES prowl_deliveries(outbox_id) ON DELETE CASCADE,
    number INTEGER NOT NULL CHECK (number > 0),
    sent_at TEXT NOT NULL,
    completed_at TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL CHECK (state IN ('sending','accepted','failed','delayed','uncertain')),
    reason TEXT NOT NULL DEFAULT '',
    provider_code INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (outbox_id, number)
) STRICT;

CREATE INDEX prowl_active_deliveries ON prowl_deliveries (route_id, lease_until, outbox_id);
CREATE INDEX prowl_attempts_recent ON prowl_attempts (sent_at DESC);
