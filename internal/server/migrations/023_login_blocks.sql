-- Addresses blocked after too many failed sign-ins (brute-force protection).
CREATE TABLE login_blocks (
    ip         TEXT PRIMARY KEY,
    until      TIMESTAMPTZ NOT NULL,
    strikes    INT NOT NULL DEFAULT 1,
    reason     TEXT NOT NULL DEFAULT '',
    blocked_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
