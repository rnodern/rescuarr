CREATE TABLE owner (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    password_hash BLOB NOT NULL,
    password_salt BLOB NOT NULL,
    password_scheme TEXT NOT NULL,
    auth_revision INTEGER NOT NULL CHECK (auth_revision > 0)
) STRICT;

CREATE TABLE setup_token (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    token_hash BLOB NOT NULL,
    purpose TEXT NOT NULL CHECK (purpose IN ('setup', 'recovery')),
    expires_at TEXT NOT NULL
) STRICT;

CREATE TABLE owner_sessions (
    token_hash BLOB PRIMARY KEY,
    revision INTEGER NOT NULL,
    csrf_secret BLOB NOT NULL,
    expires_at TEXT NOT NULL,
    idle_until TEXT NOT NULL
) STRICT;

CREATE INDEX owner_sessions_expiry ON owner_sessions (idle_until);
