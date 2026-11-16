CREATE TABLE series_episode_title_requirements (
    instance_id TEXT NOT NULL REFERENCES observe_instances(id),
    epoch INTEGER NOT NULL CHECK (epoch > 0),
    series_id INTEGER NOT NULL CHECK (series_id > 0),
    PRIMARY KEY (instance_id, epoch, series_id)
) STRICT;
