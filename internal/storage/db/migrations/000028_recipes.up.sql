-- Recipe registry (roady #125). Tracks recipes the user has installed
-- locally (built-in catalog entries don't need a row here; they live in
-- the embedded YAML). Exported recipes also land here so they're
-- browsable in the registry view.
CREATE TABLE IF NOT EXISTS recipes (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    version       TEXT NOT NULL,
    author        TEXT,
    description   TEXT,
    tags          TEXT NOT NULL DEFAULT '[]',
    yaml          TEXT NOT NULL,
    sha256        TEXT NOT NULL,
    source        TEXT NOT NULL DEFAULT 'imported',  -- 'imported' | 'exported'
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_recipes_source ON recipes(source);
