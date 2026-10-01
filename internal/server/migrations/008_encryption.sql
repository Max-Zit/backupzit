-- Repository encryption per storage target. repo_password is the recovery
-- key that unlocks every agent repository on this target.
ALTER TABLE storage_targets ADD COLUMN encrypted BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE storage_targets ADD COLUMN repo_password TEXT NOT NULL DEFAULT '';
