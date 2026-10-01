-- Reports emailed on a schedule.
CREATE TABLE report_schedules (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    frequency   TEXT NOT NULL,                 -- daily | weekly | monthly
    agent_id    BIGINT REFERENCES agents(id) ON DELETE CASCADE,
    job_id      BIGINT REFERENCES jobs(id) ON DELETE CASCADE,
    recipients  TEXT[] NOT NULL DEFAULT '{}',  -- empty: notification recipients
    hour        INTEGER NOT NULL DEFAULT 7,
    last_period TEXT NOT NULL DEFAULT '',      -- period key of the last report sent
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
