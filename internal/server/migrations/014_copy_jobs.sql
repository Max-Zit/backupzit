-- Backup copy jobs: copy the backups of another job to a second storage.
ALTER TABLE jobs ADD COLUMN source_job_id BIGINT REFERENCES jobs(id) ON DELETE CASCADE;
ALTER TABLE runs ADD COLUMN source_target_id BIGINT REFERENCES storage_targets(id) ON DELETE SET NULL;
ALTER TABLE runs ADD COLUMN source_repo_url TEXT NOT NULL DEFAULT '';
