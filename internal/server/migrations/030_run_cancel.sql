-- Cancelling a running run: the agent stops it on its next poll. cancel_by
-- is the user ('' for the job's time limit); cancel_reason becomes the run's
-- message.
ALTER TABLE runs ADD COLUMN cancel_reason TEXT;
ALTER TABLE runs ADD COLUMN cancel_by TEXT;
ALTER TABLE runs ADD COLUMN cancel_at TIMESTAMPTZ;
