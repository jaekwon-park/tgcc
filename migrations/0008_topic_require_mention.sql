-- migrations/0008_topic_require_mention.sql
-- Per-topic "require @mention to respond" flag.
-- 0 = respond to every message (default), 1 = only respond when the bot is
-- @mentioned or the message replies to one of the bot's messages.

ALTER TABLE topics ADD COLUMN require_mention INTEGER NOT NULL DEFAULT 0;
