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

## Remote (SSH) execution

A topic can run Codex on a remote host over SSH instead of a local subprocess by setting `remote_ssh_host`. This is intended for a single Windows host (e.g. a reverse-engineering workstation running Codex + Ghidra); it is not a general multi-host framework.

```toml
[[group.topic]]
thread_id             = 19350
cli                   = "codex"
model                 = "gpt-5.5"
remote_ssh_host       = "flcl@10.1.1.104"
remote_workspace_path = "C:\\Users\\FLCL\\re-analysis"
```

When `remote_ssh_host` is set, `workspace_path` is ignored (leave it empty) and `remote_workspace_path` becomes the working directory, passed to codex via `--cd`. The SSH connection must already be passwordless (key-based) from the tgcc host, and the remote host must have `codex` on its PATH. tgcc wraps the local `codex exec` command in `ssh -o BatchMode=yes -o ConnectTimeout=10 <host> "<remote command>"`; the prompt flows over SSH stdin and the JSONL stdout is parsed by the same logic as local execution. The remote command line is quoted for cmd.exe, so Windows paths and spaces are preserved.

Remote execution is minimal: photos are not forwarded to the remote host, `.notify-queue` draining does not apply, and Honcho transcript persistence relies on local rollout files and so does not capture remote conversations. SSH connection failures are reported to the user as a clear "원격 호스트 연결 실패" error. Removing `remote_ssh_host` from tgcc.toml and restarting reverts the topic to local execution.

### Wake-on-LAN auto-retry

A remote host that is powered off fails the SSH connect with `Connection timed out` / `Connection refused`. When a remote topic also sets `wol_mac` and `wol_relay_host`, tgcc wakes the host instead of failing immediately:

```toml
[[group.topic]]
thread_id             = 19350
cli                   = "codex"
model                 = "gpt-5.5"
remote_ssh_host       = "flcl@10.1.1.104"
remote_workspace_path = "C:\\Users\\FLCL\\re-analysis"
wol_mac               = "a0:ad:9f:cd:5e:8a"
wol_relay_host        = "insainty@10.1.1.21"
```

- `wol_mac` is the remote host's MAC address.
- `wol_relay_host` is an SSH host on the same L2 subnet as the remote host. tgcc itself cannot reach the remote subnet's broadcast address (Cloudflare WARP routes individual IPs only), so the relay host broadcasts the magic packet on tgcc's behalf.

On a connect failure, tgcc SSHs into `wol_relay_host` and broadcasts a magic packet — preferring the `wakeonlan` binary when present and falling back to a self-contained Python sender. It then notifies the topic ("원격 호스트를 깨우는 중입니다...") and retries the SSH connect (default 5 attempts, 15s apart). Success resumes the normal codex exec; if the host never boots, the user gets "원격 호스트가 부팅되지 않았습니다. 직접 확인이 필요합니다". Topics without `wol_mac`/`wol_relay_host` keep the previous immediate-error behavior.

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

When Honcho is enabled, /stop, /kill, /refresh, /restart and switching away from Codex attempt to save the user/assistant conversation from the native Codex rollout. The first request in a new Codex thread retrieves the topic's Honcho memory and recent conversation from the latest archived session, including a Claude predecessor. Resuming an existing thread does not inject memory again. A service restart that preserves the thread continues through Codex resume, not Honcho restoration.

Rollout lookup uses [spawn.env].CODEX_HOME, the inherited CODEX_HOME, or ~/.codex, and verifies the thread metadata. Tool/reasoning events, bootstrap instructions and previously injected memory are excluded. Existing Codex conversations can be saved without having been started by this version. Native logs must remain available; missing or unreadable logs cannot be saved. Photos are not uploaded to Honcho; text captions are preserved.

Persistence is best-effort, like Claude: a Honcho outage does not block stop or refresh. Recent archived conversation is used as a fallback when memory retrieval fails. Saves are lifecycle snapshots, not per-message ingestion, and repeated saves can duplicate history. The shared Honcho client uses v3 batch messages with peer_id, splits long text at 25,000 characters, and reads summary/messages as well as legacy context responses. Claude transcript compaction and /squash remain unsupported for Codex.

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

Migration 0009 adds topic/session CLI, Codex thread ID and process start identity. Migration 0010 adds `remote_ssh_host` and `remote_workspace_path` to topics. Migration 0011 adds `wol_mac` and `wol_relay_host`. Ship migrations beside the executable. Do not run the new binary against the live database just to test it: opening the store applies migrations.

Before deployment, back up the live WAL database with SQLite's backup API or sqlite3 .backup; copying only state.db while it is open is insufficient. Test migration and integrity/foreign-key checks on a copy, then stage the binary, SQL, TOML and service configuration. Start with one Codex topic and verify real messages, resume, images, cancellation, queue delivery, restart recovery and an existing Claude topic.

Before rolling back to an older binary, stop and archive Codex sessions with the new binary and restore topic CLI/model settings. Older binaries cannot safely interpret active Codex sessions. Preserve the database backup separately; restoring it also discards later history.
