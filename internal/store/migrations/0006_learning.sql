CREATE TABLE learning_patterns (
    id TEXT PRIMARY KEY CHECK (length(id) = 64),
    instance_id TEXT NOT NULL,
    signature TEXT NOT NULL CHECK (length(signature) <= 8192 AND json_valid(signature)),
    control_revision INTEGER NOT NULL DEFAULT 0 CHECK (control_revision >= 0),
    paused INTEGER NOT NULL DEFAULT 0 CHECK (paused IN (0, 1)),
    reset_at TEXT NOT NULL DEFAULT '',
    changed_at TEXT NOT NULL DEFAULT '',
    last_match_at TEXT NOT NULL DEFAULT '',
    saturated_at TEXT NOT NULL DEFAULT '',
    saturated_safety_at TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX learning_patterns_instance ON learning_patterns(instance_id, id);

CREATE TABLE learning_samples (
    pattern_id TEXT NOT NULL REFERENCES learning_patterns(id),
    sample_key TEXT NOT NULL CHECK (length(sample_key) = 64),
    state TEXT NOT NULL CHECK (state IN ('verified_success', 'verified_failure', 'neutral', 'contradiction')),
    recorded_at TEXT NOT NULL,
    available_at TEXT NOT NULL,
    candidate TEXT NOT NULL CHECK (length(candidate) <= 16384 AND json_valid(candidate)),
    PRIMARY KEY (pattern_id, sample_key)
) STRICT;

CREATE INDEX learning_samples_time ON learning_samples(pattern_id, recorded_at, sample_key);

CREATE TABLE learning_control_audit (
    pattern_id TEXT NOT NULL REFERENCES learning_patterns(id),
    revision INTEGER NOT NULL CHECK (revision > 0),
    event_id TEXT NOT NULL UNIQUE REFERENCES audit_events(id),
    change TEXT NOT NULL CHECK (change IN ('pause', 'resume', 'reset')),
    PRIMARY KEY (pattern_id, revision)
) STRICT;
