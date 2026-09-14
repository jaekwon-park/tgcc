ALTER TABLE topics ADD COLUMN cli TEXT NOT NULL DEFAULT 'claude' CHECK (cli IN ('claude', 'codex'));
ALTER TABLE sessions ADD COLUMN cli TEXT NOT NULL DEFAULT 'claude' CHECK (cli IN ('claude', 'codex'));
ALTER TABLE sessions ADD COLUMN codex_thread_id TEXT;
ALTER TABLE sessions ADD COLUMN codex_process_start TEXT NOT NULL DEFAULT '';
UPDATE system_meta SET value = '9' WHERE key = 'schema_version';
