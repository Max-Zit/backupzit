-- VMware ESXi hosts backed up through a proxy agent.
CREATE TABLE vmware_hosts (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    address        TEXT NOT NULL,
    username       TEXT NOT NULL,
    password       TEXT NOT NULL DEFAULT '',
    thumbprint     TEXT NOT NULL DEFAULT '',
    ssh_host_key   TEXT NOT NULL DEFAULT '',
    proxy_agent_id BIGINT REFERENCES agents(id) ON DELETE SET NULL,
    repo_dir       TEXT NOT NULL,
    inventory      JSONB,
    inventory_at   TIMESTAMPTZ,
    version        TEXT NOT NULL DEFAULT '',
    api_writes     BOOLEAN NOT NULL DEFAULT false,
    ssh_ok         BOOLEAN NOT NULL DEFAULT false,
    last_error     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE jobs ADD COLUMN vmware_host_id BIGINT REFERENCES vmware_hosts(id) ON DELETE CASCADE;
ALTER TABLE runs ADD COLUMN vmware_host_id BIGINT REFERENCES vmware_hosts(id) ON DELETE SET NULL;
