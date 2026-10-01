-- Server-wide settings, e.g. email notifications (key -> JSON value).
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Whether the alert email for a finished run has been handled.
ALTER TABLE runs ADD COLUMN notified BOOLEAN NOT NULL DEFAULT false;
-- Runs that finished before notifications existed are not mailed.
UPDATE runs SET notified = true;
