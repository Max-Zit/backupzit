-- Chat and webhook alerts of a run are sent once (chat_notified), apart
-- from the email, which is retried while the mail server fails (0.55).
ALTER TABLE runs ADD COLUMN chat_notified BOOLEAN NOT NULL DEFAULT false;
