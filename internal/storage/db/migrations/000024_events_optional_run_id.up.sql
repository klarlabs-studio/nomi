-- Make events.run_id nullable + drop the FK to runs(id).
--
-- Driven by ADR 0004 §6: entity-scoped events (assistant.deleted,
-- run.deleted, memory.store, memory.forget, memory.tombstone) target an
-- entity other than a run and carry the entity ID in the payload rather
-- than in run_id. Today those events fail to persist because run_id is
-- NOT NULL and the empty-string fallback violates the FK to runs(id).
--
-- SQLite does not support ALTER COLUMN; this rebuilds the table.
-- prev_hash / entry_hash (added in 000023) are preserved so the chain
-- continues across the migration without re-computation.

CREATE TABLE events_new (
    id TEXT PRIMARY KEY,
    type TEXT NOT NULL,
    run_id TEXT,
    step_id TEXT,
    payload TEXT NOT NULL,
    timestamp DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    prev_hash TEXT,
    entry_hash TEXT
);

INSERT INTO events_new (id, type, run_id, step_id, payload, timestamp, prev_hash, entry_hash)
SELECT id, type, run_id, step_id, payload, timestamp, prev_hash, entry_hash
FROM events;

DROP TABLE events;
ALTER TABLE events_new RENAME TO events;

CREATE INDEX idx_events_run ON events(run_id);
CREATE INDEX idx_events_type ON events(type);
CREATE INDEX idx_events_timestamp ON events(timestamp);
CREATE INDEX idx_events_entry_hash ON events(entry_hash);
