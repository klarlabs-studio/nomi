-- Hash-chain audit log columns on the events table.
--
-- Each event carries:
--   prev_hash   = entry_hash of the prior event (NULL for the first entry).
--   entry_hash  = sha256_hex(prev_hash || canonical_json(event)).
--
-- The application (EventRepository) computes both columns inside the same
-- transaction as the INSERT, with a per-process mutex serializing writes
-- so the chain is well-defined under concurrent producers.
--
-- Verification at /audit/verify walks events ORDER BY timestamp ASC, id ASC
-- and recomputes each entry_hash, returning the first inconsistency or
-- "ok @ N" if the chain is intact end to end.

ALTER TABLE events ADD COLUMN prev_hash TEXT;
ALTER TABLE events ADD COLUMN entry_hash TEXT;

CREATE INDEX IF NOT EXISTS idx_events_entry_hash ON events(entry_hash);
