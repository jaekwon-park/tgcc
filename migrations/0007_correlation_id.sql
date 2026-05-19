-- migrations/0007_correlation_id.sql
-- Add correlation_id to sessions for 1:1 matching in SessionStart hook
-- Prevents concurrent spawn race condition on same workspace

ALTER TABLE sessions ADD COLUMN correlation_id TEXT;
CREATE INDEX IF NOT EXISTS idx_sessions_correlation ON sessions(correlation_id);
