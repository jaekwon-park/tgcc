-- migrations/0004_context_overrides.sql
-- Topic-level context threshold overrides (M9)
-- Adds context_overrides TEXT (JSON) column to topics table.

ALTER TABLE topics ADD COLUMN context_overrides TEXT;

UPDATE system_meta SET value = '4' WHERE key = 'schema_version';
