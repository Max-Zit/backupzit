-- Enrollment codes the console makes for one machine (agent installs from
-- the console) work once (0.62, B9).
ALTER TABLE enrollment_tokens ADD COLUMN single_use BOOLEAN NOT NULL DEFAULT false;
