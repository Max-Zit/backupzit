CREATE TABLE tenants (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    slug        TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
    id             BIGSERIAL PRIMARY KEY,
    username       TEXT NOT NULL UNIQUE,
    password_hash  TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash  BYTEA PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE TABLE storage_targets (
    id             BIGSERIAL PRIMARY KEY,
    tenant_id      BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    kind           TEXT NOT NULL,            -- 'sftp' | 'local'
    url            TEXT NOT NULL,            -- base location; each agent gets a sub-path
    sftp_password  TEXT NOT NULL DEFAULT '',
    sftp_key       TEXT NOT NULL DEFAULT '', -- PEM private key
    sftp_host_key  TEXT NOT NULL DEFAULT '', -- SHA256:... fingerprint
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, name)
);

CREATE TABLE enrollment_tokens (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    token_hash  BYTEA NOT NULL UNIQUE,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE agents (
    id           BIGSERIAL PRIMARY KEY,
    tenant_id    BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    uuid         TEXT NOT NULL UNIQUE,
    secret_hash  BYTEA NOT NULL,
    hostname     TEXT NOT NULL,
    os           TEXT NOT NULL DEFAULT '',
    arch         TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',
    enrolled_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ
);

CREATE TABLE jobs (
    id                 BIGSERIAL PRIMARY KEY,
    tenant_id          BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    agent_id           BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    target_id          BIGINT NOT NULL REFERENCES storage_targets(id) ON DELETE RESTRICT,
    name               TEXT NOT NULL,
    paths              TEXT[] NOT NULL,
    excludes           TEXT[] NOT NULL DEFAULT '{}',
    schedule           TEXT NOT NULL DEFAULT '',  -- cron expression, '' = manual only
    enabled            BOOLEAN NOT NULL DEFAULT true,
    last_scheduled_at  TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE runs (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     BIGINT NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    job_id        BIGINT REFERENCES jobs(id) ON DELETE SET NULL,
    kind          TEXT NOT NULL,                   -- 'backup' | 'restore'
    status        TEXT NOT NULL DEFAULT 'queued',  -- queued|running|success|warning|failed
    trigger       TEXT NOT NULL DEFAULT 'manual',  -- manual|schedule
    -- what to do (copied at creation so later job edits don't change history)
    repo_url      TEXT NOT NULL,
    target_id     BIGINT REFERENCES storage_targets(id) ON DELETE SET NULL,
    paths         TEXT[] NOT NULL DEFAULT '{}',
    excludes      TEXT[] NOT NULL DEFAULT '{}',
    snapshot_id   TEXT NOT NULL DEFAULT '',        -- backup: produced; restore: source
    restore_target TEXT NOT NULL DEFAULT '',
    restore_verify BOOLEAN NOT NULL DEFAULT false,
    -- outcome
    queued_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    stats         JSONB,
    errors        TEXT[] NOT NULL DEFAULT '{}',
    message       TEXT NOT NULL DEFAULT ''
);

CREATE INDEX runs_agent_status ON runs (agent_id, status);
CREATE INDEX runs_job ON runs (job_id, queued_at DESC);
CREATE INDEX runs_queued_at ON runs (queued_at DESC);
