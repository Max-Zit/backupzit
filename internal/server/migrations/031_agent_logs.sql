-- Logs that agents sent on request (run kind agent-log); kept 30 days.
CREATE TABLE run_logs (
    run_id     BIGINT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
    data       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
