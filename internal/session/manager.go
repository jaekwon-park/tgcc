// Package session manages Claude Code session lifecycle.
package session

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/honcho"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

// Manager handles spawn/kill/resume of Claude Code sessions.
type Manager struct {
	store          *store.Store
	adapter        *tmux.Adapter
	sm             *StateMachine
	logger         *slog.Logger
	sender         *bot.Sender
	honchoClient   *honcho.HonchoClient
	tmuxSession    string
	claudeBin      string
	workspaceRoot  string            // default root for /workspaces scanning
	workspaceRoots []string          // from tgcc.toml workspace.roots
	spawnEnv       map[string]string // extra env injected into every spawned Claude
}

// NewManager creates a new session Manager.
//
// spawnEnv is merged into the tmux new-window environment for every Spawn,
// Resume, and FreshRestart. Per-spawn keys (TGCC_CORRELATION_ID) always win
// over this map. Pass nil for the legacy "no extra env" behavior.
func NewManager(st *store.Store, adapter *tmux.Adapter, logger *slog.Logger, sender *bot.Sender, honchoClient *honcho.HonchoClient, tmuxSession, claudeBin, workspaceRoot string, workspaceRoots []string, spawnEnv map[string]string) *Manager {
	if workspaceRoot == "" {
		homeDir, _ := os.UserHomeDir()
		workspaceRoot = homeDir
	}
	return &Manager{
		store:          st,
		adapter:        adapter,
		sm:             NewStateMachine(),
		logger:         logger,
		sender:         sender,
		honchoClient:   honchoClient,
		tmuxSession:    tmuxSession,
		claudeBin:      claudeBin,
		workspaceRoot:  workspaceRoot,
		workspaceRoots: workspaceRoots,
		spawnEnv:       spawnEnv,
	}
}

// buildSpawnEnv merges the configured spawn env with the per-spawn overrides.
// Per-spawn keys win — this lets TGCC_CORRELATION_ID (and any future per-call
// values) take precedence over anything seeded from tgcc.toml [spawn.env].
func (m *Manager) buildSpawnEnv(perSpawn map[string]string) map[string]string {
	if len(m.spawnEnv) == 0 && len(perSpawn) == 0 {
		return nil
	}
	out := make(map[string]string, len(m.spawnEnv)+len(perSpawn))
	for k, v := range m.spawnEnv {
		out[k] = v
	}
	for k, v := range perSpawn {
		out[k] = v
	}
	return out
}

// Spawn creates a new Claude Code session for the given topic and workspace.
// It performs: session insert → tmux new-window → claude execution → status update.
func (m *Manager) Spawn(ctx context.Context, topicID int64, workspacePath string, chatID int64, threadID int64, model string) (*store.Session, error) {
	// 1. Validate workspace exists
	if _, err := os.Stat(workspacePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace not found: %s", workspacePath)
	}

	// 2. Check for existing session on this topic. The
	// uq_sessions_active_topic index allows at most one non-archived row per
	// topic, so any dead-but-not-archived predecessor (stopped/failed/crashed)
	// must be archived before InsertSession or we'd hit the UNIQUE constraint.
	existing, err := m.store.SessionByTopicID(topicID)
	if err != nil {
		return nil, fmt.Errorf("check existing session: %w", err)
	}
	if existing != nil {
		if m.sm.IsActive(Status(existing.Status)) {
			return existing, fmt.Errorf("topic already has active session (status: %s)", existing.Status)
		}
		if err := m.store.ArchiveSession(existing.ID, store.CurrentTimeMs()); err != nil {
			return nil, fmt.Errorf("archive predecessor session %s: %w", existing.ID, err)
		}
		m.logger.Info("archived predecessor session before spawn", "session_id", existing.ID, "status", existing.Status)
	}

	// 3. Create session record
	now := store.CurrentTimeMs()
	sessionID := uuid.New().String()
	windowName := windowNameForWorkspace(workspacePath)

	sess := &store.Session{
		ID:             sessionID,
		TopicID:        topicID,
		TmuxSession:    m.tmuxSession,
		TmuxWindow:     windowName,
		WorkspacePath:  workspacePath,
		CorrelationID:  sessionID,
		Status:         string(StatusPending),
		CreatedAt:      now,
		LastActivityAt: now,
	}
	if err := m.store.InsertSession(sess); err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}

	// 4. Transition: pending → spawning
	if err := m.sm.Transition(ctx, sess, StatusSpawning); err != nil {
		// Side effects may fail but we still update status in DB
		m.logger.Warn("transition pending→spawning side effect", "error", err)
	}
	if err := m.store.UpdateSessionStatus(sessionID, string(StatusSpawning)); err != nil {
		return nil, fmt.Errorf("update status to spawning: %w", err)
	}
	sess.Status = string(StatusSpawning)

	// 5. Build claude command
	args := []string{"--dangerously-skip-permissions"}
	if model != "" {
		args = append(args, "--model", model)
	}
	env := m.buildSpawnEnv(map[string]string{
		"TGCC_CORRELATION_ID": sessionID,
	})
	claudeCmd := m.claudeBin + " " + strings.Join(args, " ")
	m.logger.Info("spawning session", "session_id", sessionID, "window", windowName, "cmd", claudeCmd)

	// 6. Spawn tmux window
	winfo, err := m.adapter.NewWindowWithEnv(m.tmuxSession, windowName, workspacePath, m.claudeBin, args, env)
	if err != nil {
		if cerr := m.store.UpdateSessionStatus(sessionID, string(StatusFailed)); cerr != nil {
			m.logger.Warn("update status to failed on spawn error", "error", cerr)
		}
		return nil, fmt.Errorf("tmux new-window: %w", err)
	}

	// 7. Update PID and tmux window ID. Persist tmux_window to DB — tmux
	// returns the stable @N pane id (e.g. "@7") which is what subsequent
	// ForwardMessage calls must target. Leaving DB on the human window NAME
	// breaks send-keys whenever Resume/FreshRestart rename it or another
	// topic happens to share the same basename.
	if winfo.ID != "" {
		sess.TmuxWindow = winfo.ID
		if err := m.store.UpdateSessionTmuxWindow(sessionID, winfo.ID); err != nil {
			m.logger.Warn("update tmux_window failed", "error", err)
		}
	}
	if err := m.store.UpdateSessionPID(sessionID, int64(winfo.PID)); err != nil {
		m.logger.Warn("update pid failed", "error", err)
	}
	sess.PID = int64(winfo.PID)

	// 8. Wait briefly for claude to initialize, then transition to active
	// (Hook-based activation comes in M4; for M2 we allow a brief startup window)
	// M4 fix: conditional update prevents stale timer from overwriting a status
	// that has already changed (e.g. the hook set it to crashed/failed).
	go func() {
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
		// Inject prior Honcho context BEFORE marking active. ForwardMessage is
		// gated on the active state, so the user's first message lands after
		// this — preserving order (context first, then the new prompt).
		if mem := m.loadHonchoContext(context.Background(), topicID); mem != "" {
			if err := m.adapter.SendKeys(sess.TmuxWindow, mem); err != nil {
				m.logger.Warn("honcho context inject on spawn failed", "session_id", sessionID, "error", err)
			} else {
				m.logger.Info("honcho context injected on spawn", "session_id", sessionID)
			}
		}
		if cerr := m.store.UpdateSessionStatusIf(sessionID, string(StatusSpawning), string(StatusActive)); cerr != nil {
			m.logger.Error("update status to active", "error", cerr)
		}
	}()

	// 9. Send "ready" notification
	m.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chatID,
		ThreadID: threadID,
		Text:     fmt.Sprintf("✅ session ready\nWorkspace: %s\nSession: %s", workspacePath, sessionID[:8]),
	})

	return sess, nil
}

// Kill forcefully terminates a session (tmux kill-window).
// Performs stopping → stopped transition.
func (m *Manager) Kill(ctx context.Context, sessionID string) error {
	sess, err := m.store.SessionByID(sessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Transition to stopping
	if err := m.sm.Transition(ctx, sess, StatusStopping); err != nil {
		return fmt.Errorf("cannot stop from %s: %w", sess.Status, err)
	}
	if err := m.store.UpdateSessionStatus(sessionID, string(StatusStopping)); err != nil {
		return err
	}

	// Persist the conversation to Honcho before destroying the session so a
	// later /new on this topic can recover the context via loadHonchoContext.
	m.saveTranscriptToHoncho(ctx, sess)

	// Kill the tmux window (target is the session itself or window)
	target := sess.TmuxWindow
	if target == "" {
		target = fmt.Sprintf("%s:%s", sess.TmuxSession, sess.TmuxWindow)
	}
	m.logger.Info("killing session window", "session_id", sessionID, "target", target)

	if err := m.adapter.KillWindow(target); err != nil {
		m.logger.Warn("kill-window error (may already be dead)", "error", err)
	}

	// Mark stopped
	if err := m.store.UpdateSessionStatus(sessionID, string(StatusStopped)); err != nil {
		return err
	}

	return nil
}

// Stop gracefully terminates a session: send-keys "/exit", wait, then kill window.
func (m *Manager) Stop(ctx context.Context, sessionID string) error {
	sess, err := m.store.SessionByID(sessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	// Transition to stopping
	if err := m.sm.Transition(ctx, sess, StatusStopping); err != nil {
		return fmt.Errorf("cannot stop from %s: %w", sess.Status, err)
	}
	if err := m.store.UpdateSessionStatus(sessionID, string(StatusStopping)); err != nil {
		return err
	}

	// Persist the conversation to Honcho before exit so a later /new on this
	// topic can recover the context via loadHonchoContext.
	m.saveTranscriptToHoncho(ctx, sess)

	// Send exit command to claude
	target := sess.TmuxWindow
	_ = m.adapter.SendKeys(target, "/exit")

	// Wait for graceful shutdown
	select {
	case <-ctx.Done():
		m.logger.Warn("stop context cancelled, forcing kill", "session_id", sessionID)
	case <-time.After(5 * time.Second):
		// Grace period expired, proceed to force kill
	}

	// Force kill the window
	if err := m.adapter.KillWindow(target); err != nil {
		m.logger.Warn("kill-window during stop", "error", err)
	}

	if err := m.store.UpdateSessionStatus(sessionID, string(StatusStopped)); err != nil {
		return err
	}

	return nil
}

// Resume attempts to resume a crashed/hibernated session using claude --resume,
// restoring the full conversation transcript. Allowed source states are
// governed by the state machine (crashed/hibernated → resuming).
func (m *Manager) Resume(ctx context.Context, sessionID string) (*store.Session, error) {
	sess, err := m.store.SessionByID(sessionID)
	if err != nil {
		return nil, fmt.Errorf("find session: %w", err)
	}
	if sess == nil {
		return nil, fmt.Errorf("session not found: %s", sessionID)
	}

	// Only resume from crashed, stopped, or failed states
	if !m.sm.CanTransition(Status(sess.Status), StatusResuming) {
		return nil, fmt.Errorf("cannot resume from status: %s", sess.Status)
	}

	// Transition to resuming
	if err := m.store.UpdateSessionStatus(sessionID, string(StatusResuming)); err != nil {
		return nil, err
	}

	// Build claude --resume command. Must include --dangerously-skip-permissions
	// just like Spawn — without it the resumed session reverts to interactive
	// permission prompts ("Do you want to proceed?") for every Bash/tool call,
	// which blocks the pane indefinitely since the operator can't see or answer
	// the prompt through Telegram.
	claudeSessionID := sess.ClaudeSessionID
	if claudeSessionID == "" {
		claudeSessionID = sess.ID
	}
	resumeArgs := []string{"--resume", claudeSessionID, "--dangerously-skip-permissions"}
	// Append model flag if topic has one configured
	topic, topicErr := m.store.TopicByID(sess.TopicID)
	if topicErr == nil && topic != nil && topic.ClaudeModel.Valid {
		resumeArgs = append(resumeArgs, "--model", topic.ClaudeModel.String)
	}

	windowName := windowNameForWorkspace(sess.WorkspacePath) + "-r"
	m.logger.Info("resuming session", "session_id", sessionID, "claude_session", claudeSessionID)

	winfo, err := m.adapter.NewWindowWithEnv(m.tmuxSession, windowName, sess.WorkspacePath, m.claudeBin, resumeArgs, m.buildSpawnEnv(nil))
	if err != nil {
		if cerr := m.store.UpdateSessionStatus(sessionID, string(StatusFailed)); cerr != nil {
			m.logger.Warn("update status to failed on resume error", "error", cerr)
		}
		return nil, fmt.Errorf("resume new-window: %w", err)
	}

	// Update window info. Resume gives the new pane a "<base>-r" name; we
	// store the stable @N pane id instead so ForwardMessage can find it
	// regardless of how tmux deduplicates names across topics.
	if winfo.ID != "" {
		sess.TmuxWindow = winfo.ID
		if err := m.store.UpdateSessionTmuxWindow(sessionID, winfo.ID); err != nil {
			m.logger.Warn("update tmux_window on resume failed", "error", err)
		}
	}
	if err := m.store.UpdateSessionPID(sessionID, int64(winfo.PID)); err != nil {
		m.logger.Warn("update pid on resume", "error", err)
	}

	// Transition to active after short startup delay
	// M4 fix: conditional update only if status is still "resuming".
	go func() {
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
		if cerr := m.store.UpdateSessionStatusIf(sessionID, string(StatusResuming), string(StatusActive)); cerr != nil {
			m.logger.Warn("update status to active on resume", "error", cerr)
		}
	}()

	return sess, nil
}

// ForwardMessage sends a text message to the Claude session via tmux send-keys.
func (m *Manager) ForwardMessage(ctx context.Context, sessionID string, text string) error {
	sess, err := m.store.SessionByID(sessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}
	if sess == nil {
		return fmt.Errorf("session not found: %s", sessionID)
	}
	if !m.sm.IsActive(Status(sess.Status)) {
		return fmt.Errorf("session is not active (status: %s)", sess.Status)
	}

	target := sess.TmuxWindow
	return m.adapter.SendKeys(target, text)
}

// GetSession returns a session by ID.
func (m *Manager) GetSession(sessionID string) (*store.Session, error) {
	return m.store.SessionByID(sessionID)
}

// GetSessionByTopic returns the session for a topic.
func (m *Manager) GetSessionByTopic(topicID int64) (*store.Session, error) {
	return m.store.SessionByTopicID(topicID)
}

// ActiveSessionCount returns the number of non-terminal sessions.
func (m *Manager) ActiveSessionCount() int {
	activeStatuses := []string{
		string(StatusPending), string(StatusSpawning), string(StatusActive),
		string(StatusIdle), string(StatusCrashed), string(StatusResuming), string(StatusStopping),
		string(StatusHibernated),
	}
	count, err := m.store.ActiveSessionCount(activeStatuses)
	if err != nil {
		return 0
	}
	return count
}

// KillWindow kills a tmux window by target (session ID or window ID).
func (m *Manager) KillWindow(target string) error {
	return m.adapter.KillWindow(target)
}

// ListActiveSessions returns all sessions that are not in a terminal state.
func (m *Manager) ListActiveSessions(ctx context.Context) ([]*store.Session, error) {
	activeStatuses := []string{
		string(StatusPending), string(StatusSpawning), string(StatusActive),
		string(StatusIdle), string(StatusCrashed), string(StatusResuming), string(StatusStopping),
		string(StatusHibernated),
	}
	return m.store.ActiveSessions(activeStatuses)
}

// ListWorkspaces scans workspace roots for available directories.
func (m *Manager) ListWorkspaces() ([]string, error) {
	roots := m.workspaceRoots
	if len(roots) == 0 {
		roots = []string{m.workspaceRoot}
	}

	dirs := make([]string, 0)
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			m.logger.Warn("read workspace root failed, skipping", "root", root, "error", err)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	return dirs, nil
}

// sanitizeWindowName creates a valid tmux window name from a path component.
func sanitizeWindowName(name string) string {
	// Replace problematic characters for tmux window names
	name = strings.ReplaceAll(name, ".", "-")
	name = strings.ReplaceAll(name, ":", "-")
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}

// windowNameForWorkspace builds the tmux window name for a workspace path. It
// joins the parent directory and basename so that two topics whose workspaces
// share a basename (e.g. `.../ccgram/general` and `.../hongbot-group/general`)
// don't collide on a duplicate "general" window — a collision tmux allows but
// that made it impossible to disambiguate panes by name. We still persist the
// stable @N pane id to the DB after window creation; the human-readable name
// is purely cosmetic but should still be unique per topic.
func windowNameForWorkspace(workspacePath string) string {
	base := filepath.Base(workspacePath)
	parent := filepath.Base(filepath.Dir(workspacePath))
	if parent == "" || parent == "." || parent == "/" || parent == "workspace" {
		return sanitizeWindowName(base)
	}
	return sanitizeWindowName(parent + "-" + base)
}

// FreshRestart archives the current session and creates a fresh session for the same topic.
// If summary is non-empty, it is sent as the first message to the new Claude session.
func (m *Manager) FreshRestart(ctx context.Context, oldSessionID string, summary string, chatID, threadID int64) (*store.Session, error) {
	// 1. Look up the old session
	oldSess, err := m.store.SessionByID(oldSessionID)
	if err != nil {
		return nil, fmt.Errorf("find old session: %w", err)
	}
	if oldSess == nil {
		return nil, fmt.Errorf("session not found: %s", oldSessionID)
	}

	// 2a. Persist the conversation to Honcho BEFORE archiving so the new Claude
	// can recover long-term memory via BuildResumeContext on the next start.
	// Best-effort: failure here must not block the restart path.
	m.saveTranscriptToHoncho(ctx, oldSess)

	// 2b. Archive the old session
	now := store.CurrentTimeMs()
	if err := m.store.ArchiveSession(oldSessionID, now); err != nil {
		return nil, fmt.Errorf("archive session: %w", err)
	}

	// 3. Kill the old tmux window (if still running)
	target := oldSess.TmuxWindow
	if target == "" {
		target = fmt.Sprintf("%s:%s", oldSess.TmuxSession, oldSess.TmuxWindow)
	}
	_ = m.adapter.KillWindow(target)

	// 4. Create new session record (same topic_id, same workspace)
	newID := uuid.New().String()
	windowName := windowNameForWorkspace(oldSess.WorkspacePath)
	newSess := &store.Session{
		ID:             newID,
		TopicID:        oldSess.TopicID,
		TmuxSession:    m.tmuxSession,
		TmuxWindow:     windowName,
		WorkspacePath:  oldSess.WorkspacePath,
		Status:         string(StatusPending),
		CreatedAt:      now,
		LastActivityAt: now,
	}
	if err := m.store.InsertSession(newSess); err != nil {
		return nil, fmt.Errorf("insert new session: %w", err)
	}

	// 5. Spawn tmux window with fresh claude
	freshArgs := []string{"--dangerously-skip-permissions"}
	if topic, err2 := m.store.TopicByID(oldSess.TopicID); err2 == nil && topic != nil && topic.ClaudeModel.Valid {
		freshArgs = append(freshArgs, "--model", topic.ClaudeModel.String)
	}
	winfo, err := m.adapter.NewWindowWithEnv(m.tmuxSession, windowName, oldSess.WorkspacePath, m.claudeBin, freshArgs, m.buildSpawnEnv(nil))
	if err != nil {
		if cerr := m.store.UpdateSessionStatus(newID, string(StatusFailed)); cerr != nil {
			m.logger.Warn("update status to failed on fresh restart error", "error", cerr)
		}
		return nil, fmt.Errorf("tmux new-window: %w", err)
	}
	// Persist the @N pane id so ForwardMessage targets the live pane (same
	// reasoning as Spawn — the windowName here is a basename that can collide
	// across topics).
	if winfo.ID != "" {
		newSess.TmuxWindow = winfo.ID
		if err := m.store.UpdateSessionTmuxWindow(newID, winfo.ID); err != nil {
			m.logger.Warn("update tmux_window on fresh restart failed", "error", err)
		}
	}
	if err := m.store.UpdateSessionPID(newID, int64(winfo.PID)); err != nil {
		m.logger.Warn("update pid failed", "error", err)
	}
	newSess.PID = int64(winfo.PID)

	// 6. Wait for claude to initialize, then send summary if provided
	// M4 fix: conditional update only if status is still "pending" (the initial state
	// set in step 4). Prevents stale timer from overwriting a session that has
	// already been changed to failed/crashed/stopped.
	go func() {
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
		if err := m.store.UpdateSessionStatusIf(newID, string(StatusPending), string(StatusActive)); err != nil {
			m.logger.Error("update status to active on fresh restart", "error", err)
		}
		if summary != "" {
			if err := m.adapter.SendKeys(newSess.TmuxWindow, summary); err != nil {
				m.logger.Warn("send summary on fresh restart failed", "error", err)
			}
		}
	}()

	// 7. Send notification
	if chatID > 0 && threadID > 0 {
		notifyText := "🔄 fresh session — 마지막 10턴 요약만 복구 (transcript 너무 큼)"
		if strings.HasPrefix(summary, "[Honcho Memory]") {
			notifyText = "🔄 fresh session + Honcho 메모리 복구"
		}
		m.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   chatID,
			ThreadID: threadID,
			Text:     notifyText,
		})
	}

	return newSess, nil
}

// deriveTranscriptPath reconstructs the Claude Code transcript path from a
// session's workspace + claude_session_id when the hook hasn't recorded one
// (Claude Code lays out ~/.claude/projects/<cwd-with-slashes-as-dashes>/<id>.jsonl).
func deriveTranscriptPath(sess *store.Session) string {
	if sess == nil || sess.ClaudeSessionID == "" || sess.WorkspacePath == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	encoded := strings.ReplaceAll(sess.WorkspacePath, "/", "-")
	return filepath.Join(home, ".claude", "projects", encoded, sess.ClaudeSessionID+".jsonl")
}

// loadHonchoContext fetches the topic's accumulated Honcho memory so it can be
// injected as the first message of a freshly spawned session, giving
// continuity across /stop→/new and crash-fresh-spawn. Returns "" when Honcho
// is disabled or has no memory for the topic.
func (m *Manager) loadHonchoContext(ctx context.Context, topicID int64) string {
	if m.honchoClient == nil || !m.honchoClient.IsEnabled() {
		return ""
	}
	topic, err := m.store.TopicByID(topicID)
	if err != nil || topic == nil {
		return ""
	}
	honchoSessionID := topic.HonchoSessionID()
	if honchoSessionID == "" {
		return ""
	}
	loadCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	mem, err := m.honchoClient.GetRepresentation(loadCtx, honchoSessionID)
	if err != nil || strings.TrimSpace(mem) == "" {
		return ""
	}
	return "[이전 세션 맥락 — Honcho 장기 기억]\n" + mem +
		"\n\n위 맥락을 참고만 하고, 이 메시지에는 응답하지 마세요. 다음 사용자 메시지를 기다리세요."
}

// saveTranscriptToHoncho writes the full assistant/user conversation from the
// session's transcript to Honcho before the session is destroyed (FreshRestart,
// Stop, Kill). The next session can pull this back via loadHonchoContext /
// BuildResumeContext.
//
// Best-effort: every error is logged at WARN and ignored — the restart path
// must not block on Honcho availability. No-op when Honcho is disabled, the
// topic lookup fails, the transcript is missing/empty, or the session has no
// extractable text turns.
func (m *Manager) saveTranscriptToHoncho(ctx context.Context, sess *store.Session) {
	if m.honchoClient == nil || !m.honchoClient.IsEnabled() {
		return
	}
	if sess == nil {
		return
	}
	transcriptPath := sess.TranscriptPath
	if transcriptPath == "" {
		transcriptPath = deriveTranscriptPath(sess)
	}
	if transcriptPath == "" {
		return
	}

	topic, err := m.store.TopicByID(sess.TopicID)
	if err != nil || topic == nil {
		m.logger.Warn("honcho save: topic lookup failed",
			"session_id", sess.ID, "topic_id", sess.TopicID, "error", err)
		return
	}
	honchoSessionID := topic.HonchoSessionID()
	if honchoSessionID == "" {
		return
	}

	text, err := m.extractFullConversation(transcriptPath)
	if err != nil {
		m.logger.Warn("honcho pre-restart save: extract failed",
			"session_id", sess.ID, "transcript", sess.TranscriptPath, "error", err)
		return
	}
	if strings.TrimSpace(text) == "" {
		return
	}

	saveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := m.honchoClient.CreateMessage(saveCtx, honchoSessionID, "user", text); err != nil {
		m.logger.Warn("honcho pre-restart save: create message failed",
			"session_id", sess.ID, "honcho_session", honchoSessionID, "error", err)
		return
	}
	m.logger.Info("honcho pre-restart save: ok",
		"session_id", sess.ID, "honcho_session", honchoSessionID, "bytes", len(text))
}

// extractFullConversation reads a JSONL transcript and returns every
// user/assistant turn rendered as plain text. Used as the payload for
// pre-restart Honcho persistence.
func (m *Manager) extractFullConversation(transcriptPath string) (string, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	type msgPayload struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type transcriptEntry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}

	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		var payload msgPayload
		if err := json.Unmarshal(entry.Message, &payload); err != nil {
			continue
		}
		var parts []string
		for _, block := range payload.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s]\n%s\n\n", entry.Type, strings.Join(parts, "\n")))
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan transcript: %w", err)
	}
	return sb.String(), nil
}

// SummarizeLastNTurns reads a JSONL transcript and returns a simple text summary
// of the last N assistant/user exchanges. Returns empty string if transcript
// cannot be read or has fewer than 2 turns.
func (m *Manager) SummarizeLastNTurns(transcriptPath string, n int) (string, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	// M3 fix: parse using the same nested structure as context/relay.go
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	type msgPayload struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type transcriptEntry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}

	type turn struct {
		role    string
		content string
	}

	var turns []turn
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024) // 10MB max line
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue // skip malformed lines
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		var payload msgPayload
		if err := json.Unmarshal(entry.Message, &payload); err != nil {
			continue
		}
		var parts []string
		for _, block := range payload.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			turns = append(turns, turn{role: entry.Type, content: strings.Join(parts, "\n")})
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("scan transcript: %w", err)
	}

	if len(turns) == 0 {
		return "", nil
	}

	// Take last N*2 turns (N exchanges = N user + N assistant messages)
	maxTurns := n * 2
	if len(turns) > maxTurns {
		turns = turns[len(turns)-maxTurns:]
	}

	var sb strings.Builder
	sb.WriteString("이전 세션에서 이어집니다. 마지막 대화 요약:\n\n")
	for _, t := range turns {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", t.role, truncateString(t.content, 2000)))
	}

	return sb.String(), nil
}

// truncateString truncates a string to maxLen characters, adding "..." if truncated.
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// SquashOldestNTurns reads a JSONL transcript, extracts the oldest N turns
// (user+assistant pairs), and formats them as a text block for Honcho compression.
// Returns: (oldestTurnsText, remainingRawLines, totalTurns, error)
func (m *Manager) SquashOldestNTurns(transcriptPath string, n int) (string, []string, int, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", nil, 0, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	// M3 fix: parse using the same nested structure as context/relay.go
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	type msgPayload struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type transcriptEntry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}

	type turn struct {
		role    string
		content string
		rawLine string
	}

	var turns []turn
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry transcriptEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.Type != "user" && entry.Type != "assistant" {
			continue
		}
		var payload msgPayload
		if err := json.Unmarshal(entry.Message, &payload); err != nil {
			continue
		}
		var parts []string
		for _, block := range payload.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			turns = append(turns, turn{role: entry.Type, content: strings.Join(parts, "\n"), rawLine: line})
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, 0, fmt.Errorf("scan transcript: %w", err)
	}

	totalTurns := len(turns)
	if totalTurns == 0 {
		return "", nil, 0, nil
	}
	if n > totalTurns {
		n = totalTurns
	}

	// Extract oldest N turns
	oldest := turns[:n]
	remaining := turns[n:]

	// Format oldest turns as readable text for Honcho
	var sb strings.Builder
	sb.WriteString("다음은 이전 대화 기록입니다. 이 내용을 핵심만 간결하게 요약해주세요:\n\n")
	for _, t := range oldest {
		sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", t.role, truncateString(t.content, 3000)))
	}

	// Collect remaining raw lines
	remainingLines := make([]string, len(remaining))
	for i, t := range remaining {
		remainingLines[i] = t.rawLine
	}

	return sb.String(), remainingLines, totalTurns, nil
}

// WriteSquashedTranscript creates a new transcript file containing:
// 1. The Honcho summary as a user message (prefixed with context marker)
// 2. The remaining (newer) raw turns
// Returns the path to the new transcript file.
func (m *Manager) WriteSquashedTranscript(originalPath string, summary string, remainingLines []string) (string, error) {
	newPath := originalPath + ".squashed"

	f, err := os.Create(newPath)
	if err != nil {
		return "", fmt.Errorf("create squashed transcript: %w", err)
	}
	defer f.Close()

	// Write Honcho summary as a user message (so Claude can see it as input)
	summaryMsg := map[string]string{
		"role":    "user",
		"content": fmt.Sprintf("[이전 대화 요약]\n%s", summary),
	}
	summaryLine, err := json.Marshal(summaryMsg)
	if err != nil {
		return "", fmt.Errorf("marshal summary: %w", err)
	}
	if _, err := fmt.Fprintln(f, string(summaryLine)); err != nil {
		return "", fmt.Errorf("write summary: %w", err)
	}

	// Write remaining raw turns
	for _, line := range remainingLines {
		if _, err := fmt.Fprintln(f, line); err != nil {
			return "", fmt.Errorf("write remaining line: %w", err)
		}
	}

	return newPath, nil
}
