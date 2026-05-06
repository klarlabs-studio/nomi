-- Add why column to step_definitions for planner rationale
ALTER TABLE step_definitions ADD COLUMN why TEXT DEFAULT '';
