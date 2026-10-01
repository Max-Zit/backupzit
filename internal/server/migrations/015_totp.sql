-- Two-factor authentication (TOTP) and recovery codes.
ALTER TABLE users ADD COLUMN totp_enabled BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN totp_secret TEXT NOT NULL DEFAULT '';   -- encrypted
ALTER TABLE users ADD COLUMN totp_last_step BIGINT NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN totp_recovery TEXT[] NOT NULL DEFAULT '{}'; -- SHA-256 of unused codes
