-- An agent removal waiting for the agent to uninstall itself: what to do
-- with its backups once it reported (keep, last, all).
CREATE TABLE agent_removals (
    agent_id     BIGINT PRIMARY KEY REFERENCES agents(id) ON DELETE CASCADE,
    run_id       BIGINT NOT NULL,
    backups      TEXT NOT NULL DEFAULT 'keep',
    requested_by TEXT NOT NULL DEFAULT '',
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
