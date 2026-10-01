-- Object Lock (immutability) retention in days for S3 targets, 0 = off.
ALTER TABLE storage_targets ADD COLUMN s3_lock_days INTEGER NOT NULL DEFAULT 0;
