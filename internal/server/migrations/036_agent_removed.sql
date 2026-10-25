-- Removed agents whose backups were kept stay as history: their backups
-- remain in Activity and can be restored to another agent, or connected to
-- the agent of a reinstalled computer. Their jobs are deleted; runs keep
-- the job's name.
ALTER TABLE agents ADD COLUMN removed_at TIMESTAMPTZ;
ALTER TABLE runs ADD COLUMN job_name TEXT NOT NULL DEFAULT '';
