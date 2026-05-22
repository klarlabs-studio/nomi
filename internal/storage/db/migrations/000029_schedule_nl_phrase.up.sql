-- Track the original natural-language phrase the user typed so the UI
-- can re-display it next to the persisted cron expression. Nullable;
-- schedules created via direct cron entry leave this empty.
ALTER TABLE schedules ADD COLUMN nl_phrase TEXT NOT NULL DEFAULT '';
