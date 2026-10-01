-- Hardened repository targets (backupzit-repo). url is hardened://host:port/path.
ALTER TABLE storage_targets ADD COLUMN hardened_key TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_targets ADD COLUMN hardened_fingerprint TEXT NOT NULL DEFAULT '';
