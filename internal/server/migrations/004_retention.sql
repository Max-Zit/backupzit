-- Retention policy per job (repo.RetentionPolicy as JSON; {} keeps all).
ALTER TABLE jobs ADD COLUMN retention JSONB NOT NULL DEFAULT '{}';

-- Backup runs whose snapshot was removed by the retention policy.
ALTER TABLE runs ADD COLUMN expired BOOLEAN NOT NULL DEFAULT false;
