CREATE TABLE action_intents (
    id TEXT PRIMARY KEY,
    proposal_id TEXT NOT NULL UNIQUE,
    actor TEXT NOT NULL,
    request_key TEXT NOT NULL,
    operation_key TEXT NOT NULL,
    proposal TEXT NOT NULL CHECK (json_valid(proposal)),
    physical_job TEXT NOT NULL,
    client TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('authorized', 'reserved', 'sending', 'acknowledged', 'rejected_before_effect', 'uncertain', 'reconciling', 'completed', 'cancelled')),
    fence INTEGER NOT NULL DEFAULT 0 CHECK (fence >= 0),
    lease_until TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE (actor, request_key)
) STRICT;

CREATE UNIQUE INDEX action_active_operation ON action_intents(operation_key)
WHERE state NOT IN ('completed', 'cancelled');

CREATE INDEX action_operation_history ON action_intents(operation_key, state);

CREATE TABLE action_scope_locks (
    kind TEXT NOT NULL CHECK (kind IN ('instance', 'job', 'client', 'target')),
    scope_key TEXT NOT NULL,
    intent_id TEXT NOT NULL REFERENCES action_intents(id),
    PRIMARY KEY (kind, scope_key)
) STRICT;

CREATE INDEX action_locks_intent ON action_scope_locks(intent_id);

CREATE TABLE action_charges (
    intent_id TEXT NOT NULL REFERENCES action_intents(id),
    kind TEXT NOT NULL CHECK (kind IN ('original_import', 'replacement_cycle', 'instance_removal', 'global_removal')),
    scope_key TEXT NOT NULL,
    charged_at TEXT NOT NULL,
    PRIMARY KEY (intent_id, kind, scope_key)
) STRICT;

CREATE INDEX action_charges_window ON action_charges(kind, scope_key, charged_at);

CREATE TABLE action_attempts (
    id TEXT PRIMARY KEY,
    intent_id TEXT NOT NULL UNIQUE REFERENCES action_intents(id),
    preflight TEXT NOT NULL CHECK (json_valid(preflight)),
    sent_at TEXT NOT NULL,
    acknowledgement TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(acknowledgement)),
    outcome TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(outcome)),
    conflicting_readback INTEGER NOT NULL DEFAULT 0 CHECK (conflicting_readback IN (0, 1))
) STRICT;

CREATE TABLE action_pause (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    paused INTEGER NOT NULL CHECK (paused IN (0, 1))
) STRICT;

INSERT INTO action_pause (singleton, paused) VALUES (1, 0);
