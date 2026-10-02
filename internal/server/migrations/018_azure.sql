-- Azure Blob Storage targets (credentials encrypted like other secrets).
ALTER TABLE storage_targets ADD COLUMN azure_key TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_targets ADD COLUMN azure_sas TEXT NOT NULL DEFAULT '';
