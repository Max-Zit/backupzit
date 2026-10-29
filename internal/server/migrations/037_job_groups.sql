-- Group jobs: one files job for several agents. The console keeps one
-- ordinary job per matching agent (jobs.group_id), so runs, history,
-- restores and reports work as for any job; the group holds the settings
-- and adds agents that join later.
CREATE TABLE job_groups (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    -- which agents: 'agents' (agent_ids), 'all', 'windows', 'linux', 'macos'
    scope       TEXT NOT NULL,
    agent_ids   BIGINT[] NOT NULL DEFAULT '{}',
    -- agents left out of an OS or 'all' scope
    exclude_ids BIGINT[] NOT NULL DEFAULT '{}',
    target_id   BIGINT NOT NULL REFERENCES storage_targets(id) ON DELETE RESTRICT,
    paths       TEXT[] NOT NULL,
    excludes    TEXT[] NOT NULL DEFAULT '{}',
    schedule    TEXT NOT NULL DEFAULT '',
    retention   JSONB NOT NULL DEFAULT '{}',
    options     JSONB NOT NULL DEFAULT '{}',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE jobs ADD COLUMN group_id BIGINT REFERENCES job_groups(id) ON DELETE CASCADE;
CREATE INDEX jobs_group ON jobs (group_id) WHERE group_id IS NOT NULL;
