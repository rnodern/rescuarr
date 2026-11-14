CREATE TABLE observe_instances (
    id TEXT PRIMARY KEY,
    epoch INTEGER NOT NULL CHECK (epoch > 0),
    product TEXT NOT NULL CHECK (product IN ('sonarr', 'radarr')),
    display_name TEXT NOT NULL,
    base_url TEXT NOT NULL,
    secret_ref TEXT NOT NULL,
    connection_revision INTEGER NOT NULL CHECK (connection_revision > 0),
    version TEXT NOT NULL DEFAULT '',
    health TEXT NOT NULL DEFAULT 'awaiting_scan',
    coverage TEXT NOT NULL DEFAULT 'unknown',
    checked_at TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 0,
    complete_scans INTEGER NOT NULL DEFAULT 0,
    UNIQUE (product, base_url)
) STRICT;

CREATE TABLE observe_cases (
    id TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES observe_instances(id),
    epoch INTEGER NOT NULL CHECK (epoch > 0),
    correlation_key TEXT NOT NULL,
    fingerprint TEXT NOT NULL,
    state TEXT NOT NULL,
    reason TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision > 0),
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    last_seen_generation INTEGER NOT NULL CHECK (last_seen_generation > 0),
    missing_complete_scans INTEGER NOT NULL DEFAULT 0 CHECK (missing_complete_scans >= 0),
    updated_at TEXT NOT NULL,
    UNIQUE (instance_id, epoch, correlation_key)
) STRICT;

CREATE INDEX observe_cases_instance ON observe_cases(instance_id, id);
CREATE INDEX observe_cases_state ON observe_cases(state, reason, id);
CREATE INDEX observe_case_retention ON observe_cases(instance_id, state, updated_at);
CREATE INDEX observe_cases_absence ON observe_cases(instance_id, epoch, missing_complete_scans, last_seen_generation);

CREATE TABLE observe_revision (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    revision INTEGER NOT NULL CHECK (revision > 0)
) STRICT;

INSERT INTO observe_revision (singleton, revision) VALUES (1, 1);
