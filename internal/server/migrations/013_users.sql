-- Users with roles, LDAP accounts and an audit log.
ALTER TABLE users ADD COLUMN role TEXT NOT NULL DEFAULT 'admin';   -- existing users are administrators
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'viewer';
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN email TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN source TEXT NOT NULL DEFAULT 'local'; -- local | ldap
ALTER TABLE users ADD COLUMN disabled BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN last_login_at TIMESTAMPTZ;
CREATE UNIQUE INDEX users_username_lower ON users (lower(username));

CREATE TABLE audit_log (
    id        BIGSERIAL PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    username  TEXT NOT NULL DEFAULT '',
    action    TEXT NOT NULL,
    detail    TEXT NOT NULL DEFAULT '',
    remote    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_log_at ON audit_log (at DESC);
