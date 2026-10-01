-- Disk image backups.

-- Disks reported by the agent (JSON array of imaging.Disk).
ALTER TABLE agents ADD COLUMN inventory JSONB;
ALTER TABLE agents ADD COLUMN inventory_at TIMESTAMPTZ;

-- 'files' or 'image'. Image jobs use image_disk and image_partitions
-- (NULL partitions = whole disk) instead of paths.
ALTER TABLE jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'files';
ALTER TABLE jobs ADD COLUMN image_disk INT;
ALTER TABLE jobs ADD COLUMN image_partitions INT[];

-- Run kinds now also include 'image-backup' and 'image-restore'.
ALTER TABLE runs ADD COLUMN image_disk INT;
ALTER TABLE runs ADD COLUMN image_partitions INT[];
ALTER TABLE runs ADD COLUMN target_disk INT;
ALTER TABLE runs ADD COLUMN keep_offline BOOLEAN NOT NULL DEFAULT false;
-- Kind specific result data, e.g. the disk layout of an image backup.
ALTER TABLE runs ADD COLUMN details JSONB;
