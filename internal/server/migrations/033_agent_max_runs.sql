-- How many runs an agent may execute at once (agents 0.39.0 and newer).
ALTER TABLE agents ADD COLUMN max_runs INT NOT NULL DEFAULT 1;
