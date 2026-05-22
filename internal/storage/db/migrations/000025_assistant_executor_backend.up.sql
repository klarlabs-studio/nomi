-- Pluggable executor backend per assistant. 'local' preserves pre-sandboxing
-- behavior (direct host exec). Future backends: docker, gvisor.
ALTER TABLE assistants ADD COLUMN executor_backend TEXT NOT NULL DEFAULT 'local';
