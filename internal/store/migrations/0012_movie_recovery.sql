CREATE TABLE movie_recovery (
    case_id TEXT PRIMARY KEY REFERENCES observe_cases(id),
    stage TEXT NOT NULL CHECK (stage IN ('sending', 'waiting', 'removed', 'search_sending', 'search_waiting', 'verified', 'uncertain', 'failed')),
    payload TEXT NOT NULL CHECK (json_valid(payload)),
    updated_at TEXT NOT NULL
) STRICT;
CREATE UNIQUE INDEX one_active_movie_recovery ON movie_recovery ((1))
    WHERE stage NOT IN ('verified', 'failed');
