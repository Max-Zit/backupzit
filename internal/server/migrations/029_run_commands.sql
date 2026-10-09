-- Output of the commands a job runs before and after its backups, also when
-- they succeed (JSON list of api.CommandResult).
ALTER TABLE runs ADD COLUMN commands JSONB;
