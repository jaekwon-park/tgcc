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
	tmuxSession    string
	claudeBin      string
	workspaceRoot  string   // default root for /workspaces scanning
	workspaceRoots []string // from tgcc.toml workspace.roots
}

// NewManager creates a new session Manager.
func NewManager(st *store.Store, adapter *tmux.Adapter, logger *slog.Logger, sender *bot.Sender, tmuxSession, claudeBin, workspaceRoot string, workspaceRoots []string) *Manager {
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
		tmuxSession:    tmuxSession,
		claudeBin:      claudeBin,
		workspaceRoot:  workspaceRoot,
		workspaceRoots: workspaceRoots,
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

	// 2. Archive the old session
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
	windowName := sanitizeWindowName(filepath.Base(oldSess.WorkspacePath))
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
	var freshModel string
	if topic, err2 := m.store.TopicByID(oldSess.TopicID); err2 == nil && topic != nil && topic.ClaudeModel.Valid {
		freshModel = topic.ClaudeModel.String
	}
	claudeCmd := buildClaudeCommand(oldSess.WorkspacePath, m.claudeBin, freshModel)
	winfo, err := m.adapter.NewWindow(m.tmuxSession, windowName, claudeCmd)
	if err != nil {
		m.store.UpdateSessionStatus(newID, string(StatusFailed))
		return nil, fmt.Errorf("tmux new-window: %w", err)
	}
	if winfo.ID != "" {
		newSess.TmuxWindow = winfo.ID
	}
	if err := m.store.UpdateSessionPID(newID, int64(winfo.PID)); err != nil {
		m.logger.Warn("update pid failed", "error", err)
	}
	newSess.PID = int64(winfo.PID)

	// 6. Wait for claude to initialize, then send summary if provided
	go func() {
		time.Sleep(3 * time.Second)
		if err := m.store.UpdateSessionStatus(newID, string(StatusActive)); err != nil {
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

// SummarizeLastNTurns reads a JSONL transcript and returns a simple text summary
// of the last N assistant/user exchanges. Returns empty string if transcript
// cannot be read or has fewer than 2 turns.
func (m *Manager) SummarizeLastNTurns(transcriptPath string, n int) (string, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

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
		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue // skip malformed lines
		}
		if msg.Role == "user" || msg.Role == "assistant" {
			turns = append(turns, turn{role: msg.Role, content: msg.Content})
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
		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.Role == "user" || msg.Role == "assistant" {
			turns = append(turns, turn{role: msg.Role, content: msg.Content, rawLine: line})
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
