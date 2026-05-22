-- Reverse of 000024_events_optional_run_id.up.sql: restore NOT NULL +
-- FK on events.run_id. Any rows with NULL run_id (entity-scoped events
-- from ADR 0004 §6) cannot satisfy NOT NULL — abort if present.

-- Refuse rollback if entity-scoped events exist; rolling back would
-- need to drop them (data loss) or invent a sentinel run_id.
SELECT CASE
    WHEN (SELECT COUNT(*) FROM events WHERE run_id IS NULL) > 0
    THEN RAISE(ABORT, 'cannot roll back: NULL run_id rows present in events; export and drop them first')
END;

CREATE TABLE events_old (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    run_id TEXT NOT NULL,
    step_id TEXT,
    payload TEXT NOT NULL,
    timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    prev_hash TEXT,
    entry_hash TEXT,

    FOREIGN KEY (run_id) REFERENCES runs(id) ON DELETE CASCADE
);

INSERT INTO events_old (id, type, run_id, step_id, payload, timestamp, prev_hash, entry_hash)
SELECT id, type, run_id, step_id, payload, timestamp, prev_hash, entry_hash
FROM events;

DROP TABLE events;
ALTER TABLE events_old RENAME TO events;

CREATE INDEX idx_events_run ON events(run_id);
CREATE INDEX idx_events_type ON events(type);
CREATE INDEX idx_events_timestamp ON events(timestamp);
CREATE INDEX idx_events_entry_hash ON events(entry_hash);
