-- Runs the console failed because their agent was silent (stale_failed):
-- the agent's result is still accepted when it arrives later, e.g. after a
-- console restart or a network outage (0.55).
ALTER TABLE runs ADD COLUMN stale_failed BOOLEAN NOT NULL DEFAULT false;
