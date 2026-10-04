-- Per-job options: upload speed limit, commands before/after the backup.
ALTER TABLE jobs ADD COLUMN options JSONB NOT NULL DEFAULT '{}';
