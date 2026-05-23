-- FTS5 index over memory.content for full-text search. Standalone
-- (not contentless) so the FTS5 row's `id` column lets us join back
-- to the memory table without needing rowid stability across vacuum.
CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
    id UNINDEXED,
    content,
    tokenize = 'porter unicode61'
);

-- Backfill from existing memory rows before triggers fire on future
-- writes. Idempotent: re-running just inserts dupes, which the
-- DELETE-then-INSERT pattern in the UPDATE trigger handles cleanly
-- on the next write.
INSERT INTO memory_fts(id, content)
SELECT id, content FROM memory
WHERE id NOT IN (SELECT id FROM memory_fts);

-- Keep memory_fts in sync with memory on every write. Triggers fire
-- inside the same transaction as the underlying mutation, so a
-- rollback drops both halves atomically.
CREATE TRIGGER IF NOT EXISTS memory_ai AFTER INSERT ON memory BEGIN
    INSERT INTO memory_fts(id, content) VALUES (new.id, new.content);
END;

CREATE TRIGGER IF NOT EXISTS memory_ad AFTER DELETE ON memory BEGIN
    DELETE FROM memory_fts WHERE id = old.id;
END;

CREATE TRIGGER IF NOT EXISTS memory_au AFTER UPDATE OF content ON memory BEGIN
    DELETE FROM memory_fts WHERE id = old.id;
    INSERT INTO memory_fts(id, content) VALUES (new.id, new.content);
END;
