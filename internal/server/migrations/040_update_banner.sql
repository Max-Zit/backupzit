-- The "new version available" banner: the version an administrator hid it
-- for and until when (five days). A newer version shows it again at once.
ALTER TABLE users ADD COLUMN update_hidden_version TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN update_hidden_until TIMESTAMPTZ;
