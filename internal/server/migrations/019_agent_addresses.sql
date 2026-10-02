-- Network addresses of agents: reported by the agent and as seen by the console.
ALTER TABLE agents ADD COLUMN local_ips TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE agents ADD COLUMN remote_addr TEXT NOT NULL DEFAULT '';
