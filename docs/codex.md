# Codex topic sessions

tgcc can run a topic with Claude in tmux or with the Codex CLI. Existing topics default to Claude. Codex support currently targets Linux; restart recovery uses /proc process identity and Unix process groups.

## Configuration

Place the Codex executable in the service PATH (including [spawn.env].PATH), or set its absolute path:

```toml
[codex]
bin = "/home/insainty/.npm-global/bin/codex"

[[group]]
chat_id = -1001234567890

[[group.topic]]
thread_id = 146
workspace_path = "/home/insainty/projects/codex-topic"
cli = "codex"
model = "gpt-5.5"
```

TGCC_CODEX_BIN in the binary directory's .env takes precedence over [codex].bin. The service user must already have a working Codex login. Ensure its Codex state directory exists and is writable under the service's ReadWritePaths; the example service includes /home/insainty/.codex. A custom CODEX_HOME needs its actual directory allowed instead.

Only claude and codex are valid CLI values. Unknown values fail startup. Codex always receives --model, defaulting to gpt-5.5 when the topic model is unset. Availability depends on the installed CLI and account. Claude model aliases are rejected for Codex, and gpt-/codex model names are rejected for Claude.

Codex runs with --dangerously-bypass-approvals-and-sandbox, following this deployment's existing direct-host, approval-free execution policy. This bypasses Codex sandboxing and interactive approval. It does not remove systemd filesystem restrictions. Execpolicy rules are an additional, version-dependent control, not a guarantee that all dangerous commands are blocked. This feature does not change global Codex rules.

## Messages and commands

Each request starts a short-lived codex exec process; later requests use exec resume with the saved thread ID. Prompts use stdin, photos use --image, and non-Git workspaces are supported. Assistant JSON events are relayed to Telegram and long messages are split.

Each session executes one request at a time, with up to 16 waiting user messages. Other topics can continue while Codex works. Full queues return an explicit rejection. Duplicate item IDs in a turn are not relayed twice. External active-writer conflicts receive at most three retries; other failures are never automatically replayed.

| Command | Codex behavior |
| --- | --- |
| /new [workspace] | Prepare a new conversation; the first message creates its thread |
| /model [name] | Show the model, or change it for the next turn without losing the thread |
| /stop, /kill | Cancel the process group and waiting requests; keep the thread |
| /resume | Enable another message on the preserved thread after stop/failure |
| /refresh, /restart | Cancel work, archive the old session, and prepare a fresh conversation |
| /status, /list, /list-archived | Include the session CLI |
| /compact, /ctxstatus, /squash, /ctxconfig | Explicitly unsupported |

Honcho history transfer and Claude transcript compaction are not implemented for Codex. Refresh and CLI changes do not translate conversation history between engines.

The .notify-queue watcher uses the same dispatch path. When enabled, a successful Codex turn checks pending notifications so writes during a busy turn are not lost. An unchanged notification file is not drained repeatedly within the same worker. The agent remains responsible for consuming its notification file.

## Restart and switching engines

The topic CLI is the requested engine; the session CLI records the engine that created the conversation. Startup archives sessions whose engine no longer matches their topic, stopping them with their original engine first. Removing cli restores claude and clears a stale model when switching engines without an explicit model. Set an appropriate model when switching back.

A Codex process does not have a reconnectable tmux output stream. Normal shutdown cancels workers and queued requests, waits for process exit, and attempts to flush cancellation notices. After an interrupted restart, the stored process PID, start time, session environment marker and process group are checked before terminating a leftover process. Ambiguous process identity fails startup rather than killing an unrelated process. Inspect the reported PID before retrying.

Interrupted turns become failed and require /resume plus a new user message. In-memory waiting requests do not survive a crash. Check previous side effects before resending; exactly-once execution and replay are not provided. Children that deliberately detach from the process group are outside this cleanup mechanism.

## Build, validation and deployment

```sh
go build ./...
go vet ./...
go test -race -cover ./...
```

Tests use fake Codex subprocesses and isolated SQLite databases. They cover thread continuity, models, photos, serial queues, independent topics, cancellation, writer retries, malformed/large output, commands, CLI switching and orphan recovery. They do not call a model or the live Telegram API.

Migration 0009 adds topic/session CLI, Codex thread ID and process start identity. Ship migrations beside the executable. Do not run the new binary against the live database just to test it: opening the store applies migrations.

Before deployment, back up the live WAL database with SQLite's backup API or sqlite3 .backup; copying only state.db while it is open is insufficient. Test migration and integrity/foreign-key checks on a copy, then stage the binary, SQL, TOML and service configuration. Start with one Codex topic and verify real messages, resume, images, cancellation, queue delivery, restart recovery and an existing Claude topic.

Before rolling back to an older binary, stop and archive Codex sessions with the new binary and restore topic CLI/model settings. Older binaries cannot safely interpret active Codex sessions. Preserve the database backup separately; restoring it also discards later history.
