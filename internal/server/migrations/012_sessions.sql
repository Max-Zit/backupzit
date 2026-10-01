-- Session lifetime and inactivity timeout (settings key "sessions").
ALTER TABLE sessions ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
ALTER TABLE sessions ADD COLUMN last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now();
