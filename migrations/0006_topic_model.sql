-- migrations/0006_topic_model.sql
-- Per-topic Claude model configuration
-- NULL = use default model (no --model flag)
-- non-NULL = claude --model {claude_model}

ALTER TABLE topics ADD COLUMN claude_model TEXT;
