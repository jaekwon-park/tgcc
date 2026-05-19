-- migrations/0002_context_lifecycle.sql
-- Context lifecycle columns for sessions table
-- Adds monitoring fields: transcript_bytes, turn_count, compact_count, last_compact_at

ALTER TABLE sessions ADD COLUMN transcript_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN turn_count       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN compact_count    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN last_compact_at  INTEGER;

UPDATE system_meta SET value = '2' WHERE key = 'schema_version';
