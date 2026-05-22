-- Container image used when the assistant's executor_backend is a
-- container backend (docker, gvisor). Empty for local backend.
ALTER TABLE assistants ADD COLUMN sandbox_image TEXT NOT NULL DEFAULT '';
