-- SMB storage targets. url is smb://user@host/share/path.
ALTER TABLE storage_targets ADD COLUMN smb_password TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_targets ADD COLUMN smb_domain TEXT NOT NULL DEFAULT '';
