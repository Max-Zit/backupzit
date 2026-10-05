-- Space a repository takes after each backup, and the capacity of its
-- storage when known: storage charts and forecasts.
ALTER TABLE runs ADD COLUMN repo_bytes BIGINT;
ALTER TABLE runs ADD COLUMN storage_total BIGINT;
ALTER TABLE runs ADD COLUMN storage_free BIGINT;
CREATE INDEX runs_repo_bytes ON runs(target_id, finished_at) WHERE repo_bytes IS NOT NULL;
