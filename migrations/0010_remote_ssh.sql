-- migrations/0010_remote_ssh.sql
-- Optional remote(SSH) execution for Codex topics.
-- remote_ssh_host:       user@host to run codex exec over SSH (e.g. "flcl@10.1.1.104").
--                        NULL/empty = run codex locally (existing behavior).
-- remote_workspace_path: working directory on the remote host (Windows path,
--                        e.g. "C:\Users\FLCL\re-analysis").

ALTER TABLE topics ADD COLUMN remote_ssh_host TEXT;
ALTER TABLE topics ADD COLUMN remote_workspace_path TEXT;

UPDATE system_meta SET value = '10' WHERE key = 'schema_version';
