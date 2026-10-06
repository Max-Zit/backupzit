-- Tamper-evident audit log: every entry carries the HMAC of its content and
-- of the previous entry's hash (see auditchain.go). Older entries keep ''.
ALTER TABLE audit_log ADD COLUMN prev_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE audit_log ADD COLUMN hash TEXT NOT NULL DEFAULT '';
