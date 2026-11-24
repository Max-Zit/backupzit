-- Storage reachable by agents only (0.60, #63): a NAS, SFTP server or
-- hardened repository on a customer's network that the console cannot
-- reach. The console never connects to it: agents test and search it, it
-- is not offered for the console backup, and the console does not browse
-- the files of its backups.
ALTER TABLE storage_targets ADD COLUMN agents_only BOOLEAN NOT NULL DEFAULT false;
