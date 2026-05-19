-- migrations/0003_session_archive.sql
-- Session archiving support for crash recovery + idle hibernate + /refresh
-- Replaces UNIQUE(topic_id) constraint with partial index WHERE archived_at IS NULL
-- so multiple archived sessions can coexist for the same topic.

-- H4 fix: disable FK checks during table rebuild (message_offsets references sessions).
-- Foreign keys would block DROP TABLE sessions while message_offsets still exists.
PRAGMA foreign_keys=OFF;

-- Step 1: Create new sessions table with archived_at and without the UNIQUE(topic_id)
CREATE TABLE IF NOT EXISTS sessions_new (
    id              TEXT PRIMARY KEY,
    topic_id        INTEGER NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    tmux_session    TEXT NOT NULL,
    tmux_window     TEXT NOT NULL,
    workspace_path  TEXT NOT NULL,
    claude_session_id TEXT,
    pid             INTEGER,
    status          TEXT NOT NULL CHECK(status IN (
                      'pending', 'spawning', 'active', 'idle',
                      'crashed', 'resuming', 'stopping', 'stopped', 'failed',
                      'hibernated', 'compacting'
                    )),
    last_activity_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    archived_at     INTEGER,
    transcript_path  TEXT,
    transcript_bytes INTEGER NOT NULL DEFAULT 0,
    turn_count       INTEGER NOT NULL DEFAULT 0,
    compact_count    INTEGER NOT NULL DEFAULT 0,
    last_compact_at  INTEGER
);

-- Step 2: Copy existing data
INSERT INTO sessions_new
    (id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, pid,
     status, last_activity_at, created_at, transcript_path, transcript_bytes, turn_count, compact_count, last_compact_at)
SELECT id, topic_id, tmux_session, tmux_window, workspace_path, claude_session_id, pid,
       status, last_activity_at, created_at, NULL, transcript_bytes, turn_count, compact_count, last_compact_at
FROM sessions;

-- Step 3: Drop old table and rename
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;

-- Step 4: Create partial unique index — only one active (non-archived) session per topic
CREATE UNIQUE INDEX uq_sessions_active_topic ON sessions(topic_id) WHERE archived_at IS NULL;

-- Step 5: Recreate other indexes
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_activity ON sessions(last_activity_at);

-- Step 6: Update schema version
UPDATE system_meta SET value = '3' WHERE key = 'schema_version';

-- H4 fix: re-enable foreign keys after table rebuild completes.
PRAGMA foreign_keys=ON;
