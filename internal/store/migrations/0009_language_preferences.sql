CREATE TABLE language_preferences (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    preferred_language TEXT NOT NULL CHECK (length(preferred_language) BETWEEN 2 AND 3 AND preferred_language NOT GLOB '*[^a-z]*'),
    foreign_audio TEXT NOT NULL CHECK (foreign_audio IN ('original', 'prefer_dub')),
    unknown_audio TEXT NOT NULL CHECK (unknown_audio IN ('mode_default', 'review', 'accept', 'reject')),
    prefer_single_audio INTEGER NOT NULL CHECK (prefer_single_audio IN (0, 1)),
    revision INTEGER NOT NULL CHECK (revision > 0)
) STRICT;
