-- Several proxy agents per VMware connection (0.55): the console hands
-- each VM to a proxy on the ESXi host it runs on (hot-add), the rest to
-- the connection's proxy (proxy_agent_id).
-- machine_uuid: the BIOS UUID an agent reports (a proxy's VM in vCenter).
ALTER TABLE agents ADD COLUMN machine_uuid TEXT NOT NULL DEFAULT '';
CREATE TABLE vmware_proxies (
    host_id  BIGINT NOT NULL REFERENCES vmware_hosts(id) ON DELETE CASCADE,
    agent_id BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    PRIMARY KEY (host_id, agent_id)
);
-- A VMware job run split over its proxies: part tags the snapshots of each
-- proxy (retention per part), batch joins the parts of one start (chains
-- follow once all of them finished).
ALTER TABLE runs ADD COLUMN part TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN batch BIGINT;
CREATE INDEX runs_batch ON runs(batch) WHERE batch IS NOT NULL;
-- chained: the first run of a batch once the chain after it was decided
-- (the last two parts may finish at the same time).
ALTER TABLE runs ADD COLUMN chained BOOLEAN NOT NULL DEFAULT false;
