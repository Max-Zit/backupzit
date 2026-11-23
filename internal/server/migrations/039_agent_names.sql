-- A name to show for an agent, set in the console (several customers often
-- use the same host names), and free labels for finding agents. The host
-- name the computer reports stays in hostname; repo_dir never changes.
ALTER TABLE agents ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN tags TEXT[] NOT NULL DEFAULT '{}';
