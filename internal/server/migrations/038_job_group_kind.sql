-- Jobs for several agents can also be disk image jobs (all internal disks of
-- every Windows agent) and Linux system jobs (every Linux agent).
ALTER TABLE job_groups ADD COLUMN kind TEXT NOT NULL DEFAULT 'files';
