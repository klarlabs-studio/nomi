-- Scheduled runs (roady #124). A Schedule fires a Run on a cron cadence
-- against a chosen assistant with a fixed prompt. Next-fire is computed
-- on schedule create/update and after each successful trigger; the
-- scheduler ticker queries schedules whose next_fire_at has elapsed.
CREATE TABLE IF NOT EXISTS schedules (
    id            TEXT PRIMARY KEY,
    assistant_id  TEXT NOT NULL REFERENCES assistants(id) ON DELETE CASCADE,
    prompt        TEXT NOT NULL,
    cron_expr     TEXT NOT NULL,
    enabled       INTEGER NOT NULL DEFAULT 1,
    next_fire_at  TIMESTAMP NOT NULL,
    last_fire_at  TIMESTAMP,
    last_run_id   TEXT,
    last_error    TEXT,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_schedules_next_fire ON schedules(next_fire_at)
    WHERE enabled = 1;
