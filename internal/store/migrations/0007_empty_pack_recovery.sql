CREATE TABLE empty_pack_recovery (
    case_id TEXT NOT NULL,
    instance_id TEXT NOT NULL REFERENCES observe_instances(id),
    instance_epoch INTEGER NOT NULL CHECK (instance_epoch > 0),
    job_id TEXT NOT NULL,
    job_generation INTEGER NOT NULL CHECK (job_generation > 0),
    connection_revision INTEGER NOT NULL CHECK (connection_revision > 0),
    release TEXT NOT NULL,
    download_id TEXT NOT NULL,
    queue_ids TEXT NOT NULL CHECK (json_type(queue_ids) = 'array' AND json_array_length(queue_ids) BETWEEN 1 AND 128),
    series_id INTEGER NOT NULL CHECK (series_id > 0),
    episode_ids TEXT NOT NULL CHECK (json_type(episode_ids) = 'array' AND json_array_length(episode_ids) BETWEEN 1 AND 128),
    search_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_type(search_ids) = 'array' AND json_array_length(search_ids) <= 128),
    command_id INTEGER NOT NULL DEFAULT 0 CHECK (command_id >= 0),
    stage TEXT NOT NULL CHECK (stage IN ('reject_sending', 'reject_verified', 'search_sending', 'done')),
    started_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (instance_id, instance_epoch, job_id, job_generation),
    CHECK (
        (stage IN ('reject_sending', 'reject_verified') AND json_array_length(search_ids) = 0 AND command_id = 0)
        OR (stage = 'search_sending' AND json_array_length(search_ids) > 0 AND command_id = 0)
        OR (stage = 'done' AND (json_array_length(search_ids) > 0) = (command_id > 0))
    )
) STRICT;

CREATE UNIQUE INDEX empty_pack_recovery_active_instance ON empty_pack_recovery(instance_id) WHERE stage <> 'done';
CREATE INDEX empty_pack_recovery_pending ON empty_pack_recovery(updated_at, case_id)
WHERE stage IN ('reject_sending', 'reject_verified');
