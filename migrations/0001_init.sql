-- migrations/0001_init.sql
-- Initial schema for tgcc — all tables as defined in docs/02_ARCHITECTURE.md §5

PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;

-- users: authenticated users (v0.1: single owner)
CREATE TABLE IF NOT EXISTS users (
    user_id       INTEGER PRIMARY KEY,
    username      TEXT,
    role          TEXT NOT NULL CHECK(role IN ('owner', 'unverified')),
    created_at    INTEGER NOT NULL,
    last_seen_at  INTEGER
);

-- pairing_codes: 6-digit one-time code (10 min TTL)
CREATE TABLE IF NOT EXISTS pairing_codes (
    code        TEXT PRIMARY KEY,
    user_id     INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,
    used_at     INTEGER,
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pairing_expires ON pairing_codes(expires_at);

-- chats: registered telegram chats/groups
CREATE TABLE IF NOT EXISTS chats (
    chat_id      INTEGER PRIMARY KEY,
    title        TEXT,
    is_forum     INTEGER NOT NULL DEFAULT 1,
    registered_by INTEGER NOT NULL REFERENCES users(user_id),
    registered_at INTEGER NOT NULL
);

-- topics: telegram forum topics
CREATE TABLE IF NOT EXISTS topics (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id         INTEGER NOT NULL REFERENCES chats(chat_id),
    thread_id       INTEGER NOT NULL,
    name            TEXT,
    workspace_path  TEXT,
    created_at      INTEGER NOT NULL,
    UNIQUE(chat_id, thread_id)
);
CREATE INDEX IF NOT EXISTS idx_topics_chat ON topics(chat_id);

-- sessions: Claude Code session instances
CREATE TABLE IF NOT EXISTS sessions (
    id              TEXT PRIMARY KEY,
    topic_id        INTEGER NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    tmux_session    TEXT NOT NULL,
    tmux_window     TEXT NOT NULL,
    workspace_path  TEXT NOT NULL,
    claude_session_id TEXT,
    pid             INTEGER,
    status          TEXT NOT NULL CHECK(status IN (
                      'pending', 'spawning', 'active', 'idle',
                      'crashed', 'resuming', 'stopping', 'stopped', 'failed'
                    )),
    last_activity_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE(topic_id)
);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_activity ON sessions(last_activity_at);

-- message_offsets: per-session message tracking (deduplication)
CREATE TABLE IF NOT EXISTS message_offsets (
    session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    last_hook_event_id TEXT,
    last_capture_hash  TEXT,
    updated_at  INTEGER NOT NULL
);

-- audit_log: all ACL decisions and major state changes
CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   INTEGER NOT NULL,
    actor_user_id INTEGER,
    event_type  TEXT NOT NULL,
    resource    TEXT,
    detail      TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(timestamp);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor_user_id);

-- system_meta: single-row key/value store
CREATE TABLE IF NOT EXISTS system_meta (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);

INSERT OR IGNORE INTO system_meta (key, value) VALUES ('schema_version', '1');
