// Package session manages Claude Code session lifecycle.
package session

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

// Manager handles spawn/kill/resume of Claude Code sessions.
type Manager struct {
	store         *store.Store
	adapter       *tmux.Adapter
	sm            *StateMachine
	logger        *slog.Logger
	sender        *bot.Sender
	tmuxSession   string
	claudeBin     string
	workspaceRoot string // default root for /workspaces scanning
}

// NewManager creates a new session Manager.
func NewManager(st *store.Store, adapter *tmux.Adapter, logger *slog.Logger, sender *bot.Sender, tmuxSession, claudeBin, workspaceRoot string) *Manager {
	if workspaceRoot == "" {
		homeDir, _ := os.UserHomeDir()
		workspaceRoot = homeDir
	}
	return &Manager{
		store:         st,
		adapter:       adapter,
		sm:            NewStateMachine(),
		logger:        logger,
		sender:        sender,
		tmuxSession:   tmuxSession,
		claudeBin:     claudeBin,
		workspaceRoot: workspaceRoot,
	}
}

// Spawn creates a new Claude Code session for the given topic and workspace.
// It performs: session insert → tmux new-window → claude execution → status update.
func (m *Manager) Spawn(ctx context.Context, topicID int64, workspacePath string, chatID int64, threadID int64, model string) (*store.Session, error) {
	// 1. Validate workspace exists
	if _, err := os.Stat(workspacePath); os.IsNotExist(err) {
		return nil, fmt.Errorf("workspace not found: %s", workspacePath)
	}

	// 2. Check for existing active session on this topic
	existing, err := m.store.SessionByTopicID(topicID)
	if err != nil {
		return nil, fmt.Errorf("check existing session: %w", err)
	}
	if existing != nil && m.sm.IsActive(Status(existing.Status)) {
		return existing, fmt.Errorf("topic already has active session (status: %s)", existing.Status)
	}

	// 3. Create session record
	now := store.CurrentTimeMs()
	sessionID := uuid.New().String()
	windowName := sanitizeWindowName(filepath.Base(workspacePath))

	sess := &store.Session{
		ID:             sessionID,
		TopicID:        topicID,
		TmuxSession:    m.tmuxSession,
		TmuxWindow:     windowName,
		WorkspacePath:  workspacePath,
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
	claudeCmd := buildClaudeCommand(workspacePath, m.claudeBin, model)
	m.logger.Info("spawning session", "session_id", sessionID, "window", windowName, "cmd", claudeCmd)

	// 6. Spawn tmux window
	winfo, err := m.adapter.NewWindow(m.tmuxSession, windowName, claudeCmd)
	if err != nil {
		m.store.UpdateSessionStatus(sessionID, string(StatusFailed))
		return nil, fmt.Errorf("tmux new-window: %w", err)
	}

	// 7. Update PID and tmux window ID
	if winfo.ID != "" {
		sess.TmuxWindow = winfo.ID
	}
	if err := m.store.UpdateSessionPID(sessionID, int64(winfo.PID)); err != nil {
		m.logger.Warn("update pid failed", "error", err)
	}
	sess.PID = int64(winfo.PID)

	// 8. Wait briefly for claude to initialize, then transition to active
	// (Hook-based activation comes in M4; for M2 we allow a brief startup window)
	go func() {
		time.Sleep(2 * time.Second)
		if cerr := m.store.UpdateSessionStatus(sessionID, string(StatusActive)); cerr != nil {
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

// Resume attempts to resume a crashed/stopped/failed session using claude --resume.
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

	// Build claude --resume command
	claudeSessionID := sess.ClaudeSessionID
	if claudeSessionID == "" {
		claudeSessionID = sess.ID
	}
	resumeCmd := fmt.Sprintf("cd %s && %s --resume %s",
		sess.WorkspacePath, m.claudeBin, claudeSessionID)
	// Append model flag if topic has one configured
	topic, topicErr := m.store.TopicByID(sess.TopicID)
	if topicErr == nil && topic != nil && topic.ClaudeModel.Valid {
		resumeCmd += fmt.Sprintf(" --model %s", topic.ClaudeModel.String)
	}

	windowName := sanitizeWindowName(filepath.Base(sess.WorkspacePath)) + "-r"
	m.logger.Info("resuming session", "session_id", sessionID, "claude_session", claudeSessionID)

	winfo, err := m.adapter.NewWindow(m.tmuxSession, windowName, resumeCmd)
	if err != nil {
		m.store.UpdateSessionStatus(sessionID, string(StatusFailed))
		return nil, fmt.Errorf("resume new-window: %w", err)
	}

	// Update window info
	if winfo.ID != "" {
		sess.TmuxWindow = winfo.ID
	}
	if err := m.store.UpdateSessionPID(sessionID, int64(winfo.PID)); err != nil {
		m.logger.Warn("update pid on resume", "error", err)
	}

	// Transition to active after short startup delay
	go func() {
		time.Sleep(2 * time.Second)
		m.store.UpdateSessionStatus(sessionID, string(StatusActive))
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

// ListActiveSessions returns all sessions that are not in a terminal state.
func (m *Manager) ListActiveSessions(ctx context.Context) ([]*store.Session, error) {
	activeStatuses := []string{
		string(StatusPending), string(StatusSpawning), string(StatusActive),
		string(StatusIdle), string(StatusCrashed), string(StatusResuming), string(StatusStopping),
	}
	return m.store.ActiveSessions(activeStatuses)
}

// ListWorkspaces scans workspaceRoot for available directories.
// TODO: use tgcc.toml workspace.roots when TOML parsing is implemented.
func (m *Manager) ListWorkspaces() ([]string, error) {
	entries, err := os.ReadDir(m.workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("read workspace root %s: %w", m.workspaceRoot, err)
	}

	dirs := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dirs = append(dirs, filepath.Join(m.workspaceRoot, entry.Name()))
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

// buildClaudeCommand constructs the shell command to spawn claude.
func buildClaudeCommand(workspacePath, claudeBin, model string) string {
	if claudeBin == "" {
		claudeBin = "claude"
	}
	cmd := fmt.Sprintf("cd %s && %s --dangerously-skip-permissions", workspacePath, claudeBin)
	if model != "" {
		cmd += fmt.Sprintf(" --model %s", model)
	}
	return cmd
}
