ALTER TABLE manual_reviews ADD COLUMN action TEXT NOT NULL DEFAULT 'empty_pack'
    CHECK (action IN ('empty_pack', 'import'));

CREATE TABLE manual_import_attempts (
    case_id TEXT PRIMARY KEY REFERENCES manual_reviews(case_id),
    token TEXT NOT NULL UNIQUE,
    stage TEXT NOT NULL CHECK (stage IN ('sending', 'uncertain', 'verified', 'failed', 'unconfirmed')),
    payload TEXT NOT NULL CHECK (json_valid(payload) AND json_type(payload) = 'object')
) STRICT;
