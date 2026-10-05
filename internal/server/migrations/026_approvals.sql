-- Four-eyes approval: destructive changes wait until a second user approves.
CREATE TABLE approvals (
    id           BIGSERIAL PRIMARY KEY,
    action       TEXT NOT NULL,
    object_id    BIGINT NOT NULL DEFAULT 0,
    object_name  TEXT NOT NULL DEFAULT '',
    payload      JSONB NOT NULL DEFAULT '{}',
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    status       TEXT NOT NULL DEFAULT 'pending',
    decided_by   TEXT,
    decided_at   TIMESTAMPTZ,
    note         TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX approvals_one_pending ON approvals(action, object_id) WHERE status = 'pending';
