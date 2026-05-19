-- tgcc v0.1 initial schema
-- users: authenticated users (v0.1: single owner)
CREATE TABLE IF NOT EXISTS users (
    user_id       INTEGER PRIMARY KEY,           -- telegram user_id
    username      TEXT,                          -- @handle (cache)
    role          TEXT NOT NULL                  -- 'owner' (v0.1)
                  CHECK(role IN ('owner', 'unverified')),
    created_at    INTEGER NOT NULL,              -- unix ms
    last_seen_at  INTEGER
);

-- pairing_codes: 6-digit one-time code (10min TTL)
CREATE TABLE IF NOT EXISTS pairing_codes (
    code        TEXT PRIMARY KEY,                -- 6-digit number
    user_id     INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL,                -- unix ms
    used_at     INTEGER,                         -- set when used (NULL = unused)
    created_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pairing_expires ON pairing_codes(expires_at);

-- chats: registered Telegram chats (groups/supergroups)
CREATE TABLE IF NOT EXISTS chats (
    chat_id      INTEGER PRIMARY KEY,            -- telegram chat_id (negative)
    title        TEXT,
    is_forum     INTEGER NOT NULL DEFAULT 1,     -- Forum Topics enabled?
    registered_by INTEGER NOT NULL REFERENCES users(user_id),
    registered_at INTEGER NOT NULL
);

-- topics: Telegram Forum Topics
CREATE TABLE IF NOT EXISTS topics (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id         INTEGER NOT NULL REFERENCES chats(chat_id),
    thread_id       INTEGER NOT NULL,            -- telegram message_thread_id
    name            TEXT,                        -- cache
    workspace_path  TEXT,                        -- absolute path (null = not selected)
    created_at      INTEGER NOT NULL,
    UNIQUE(chat_id, thread_id)
);
CREATE INDEX IF NOT EXISTS idx_topics_chat ON topics(chat_id);

-- sessions: Claude Code session instances
CREATE TABLE IF NOT EXISTS sessions (
    id              TEXT PRIMARY KEY,            -- UUID
    topic_id        INTEGER NOT NULL REFERENCES topics(id) ON DELETE CASCADE,
    tmux_session    TEXT NOT NULL,               -- tmux session name
    tmux_window     TEXT NOT NULL,               -- tmux window name or idx
    workspace_path  TEXT NOT NULL,
    claude_session_id TEXT,                      -- Claude Code internal session_id (for --resume)
    pid             INTEGER,                     -- last known PID
    status          TEXT NOT NULL                -- state machine reference
                    CHECK(status IN (
                      'pending', 'spawning', 'active', 'idle',
                      'crashed', 'resuming', 'stopping', 'stopped', 'failed'
                    )),
    last_activity_at INTEGER NOT NULL,
    created_at      INTEGER NOT NULL,
    UNIQUE(topic_id)                             -- one active session per topic
);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_activity ON sessions(last_activity_at);

-- message_offsets: last sent message tracking per topic (dedup)
CREATE TABLE IF NOT EXISTS message_offsets (
    session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    last_hook_event_id TEXT,                     -- hook payload event id
    last_capture_hash  TEXT,                     -- capture-pane last hash (fallback)
    updated_at  INTEGER NOT NULL
);

-- audit_log: all ACL decisions and major state changes
CREATE TABLE IF NOT EXISTS audit_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   INTEGER NOT NULL,                -- unix ms
    actor_user_id INTEGER,                       -- null = system
    event_type  TEXT NOT NULL,                   -- 'auth.denied', 'session.spawn', ...
    resource    TEXT,                            -- 'session:abc-123', 'topic:5'
    detail      TEXT                             -- JSON
);
CREATE INDEX IF NOT EXISTS idx_audit_time ON audit_log(timestamp);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor_user_id);

-- system_meta: single-row key/value (schema_version, bot_token_hash, etc.)
CREATE TABLE IF NOT EXISTS system_meta (
    key    TEXT PRIMARY KEY,
    value  TEXT NOT NULL
);
