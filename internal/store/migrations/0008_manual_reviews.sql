CREATE TABLE manual_reviews (
    case_id TEXT PRIMARY KEY,
    token TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL CHECK (state IN ('pending', 'approved', 'declined', 'completed')),
    case_revision INTEGER NOT NULL CHECK (case_revision > 0),
    search_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(search_ids) AND json_type(search_ids) = 'array'),
    scope_hash TEXT NOT NULL DEFAULT '',
    actor TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
) STRICT;

CREATE UNIQUE INDEX manual_reviews_active ON manual_reviews ((1))
WHERE state IN ('pending', 'approved');

ALTER TABLE empty_pack_recovery ADD COLUMN manual_token TEXT NOT NULL DEFAULT '';
