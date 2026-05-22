-- Optional embedding model for a provider. When set, the resolver builds
-- an EmbeddingClient against the provider's endpoint using this model id
-- (e.g. "text-embedding-3-small" for OpenAI, "nomic-embed-text" for
-- Ollama). Empty disables embedding-backed paths for the provider.
ALTER TABLE provider_profiles ADD COLUMN embedding_model_id TEXT NOT NULL DEFAULT '';
