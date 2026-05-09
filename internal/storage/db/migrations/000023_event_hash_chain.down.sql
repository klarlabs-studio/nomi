DROP INDEX IF EXISTS idx_events_entry_hash;
ALTER TABLE events DROP COLUMN entry_hash;
ALTER TABLE events DROP COLUMN prev_hash;
