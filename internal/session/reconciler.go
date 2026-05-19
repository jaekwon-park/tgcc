package session

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

// NotificationSender is the interface for sending Telegram notifications.
type NotificationSender interface {
	SendToTopic(chatID int64, threadID int64, message string) error
}

// Reconciler synchronizes SQLite session state with actual tmux state at boot time.
type Reconciler struct {
	store  *store.Store
	tmux   *tmux.Adapter
	sender NotificationSender
}

// NewReconciler creates a new Reconciler.
func NewReconciler(st *store.Store, tmuxAdapter *tmux.Adapter, sender NotificationSender) *Reconciler {
	return &Reconciler{
		store:  st,
		tmux:   tmuxAdapter,
		sender: sender,
	}
}

// ReconcileResult holds the outcome of reconciling a single session.
type ReconcileResult struct {
	SessionID string
	Action    string // "recovered", "crashed", "lost"
	TopicID   int
	Message   string
}

// Reconcile runs the boot-time reconciliation.
//
// Process:
//  1. Query SQLite for sessions with status IN (active, idle, resuming)
//  2. Check tmux list-windows for actual window existence + process liveness
//  3. For each session:
//     - window exists + process alive → status=active, memory cache ready
//     - window exists + process dead → status=crashed, notify
//     - window gone → status=failed, notify "session lost"
//
// Returns the list of reconcile results.
// TODO: Phase 3 implementation - delegate to sub-model
func (r *Reconciler) Reconcile(ctx context.Context) ([]ReconcileResult, error) {
	slog.Info("reconciler: starting boot-time reconciliation")

	// 1. Get active/idle/resuming sessions from SQLite
	sessions, err := r.store.GetActiveSessions()
	if err != nil {
		return nil, fmt.Errorf("reconciler: query sessions: %w", err)
	}

	if len(sessions) == 0 {
		slog.Info("reconciler: no sessions to reconcile")
		return nil, nil
	}

	slog.Info("reconciler: found sessions to reconcile", "count", len(sessions))

	// 2. Get actual tmux state
	tmuxData, err := r.tmux.ListWindows(ctx, "")
	if err != nil {
		// tmux server might not be running - all sessions are lost
		slog.Warn("reconciler: tmux list-windows failed, marking all sessions as failed", "error", err)
		return r.markAllFailed(ctx, sessions)
	}

	windows := tmux.ParseListWindows(tmuxData)
	windowMap := buildWindowMap(windows)

	// 3. Reconcile each session
	var results []ReconcileResult
	for _, s := range sessions {
		result := r.reconcileSession(ctx, s, windowMap)
		results = append(results, result)
	}

	slog.Info("reconciler: reconciliation complete", "total", len(sessions), "results", len(results))
	return results, nil
}

// buildWindowMap creates a lookup map from tmux window information.
func buildWindowMap(windows []tmux.TmuxWindowInfo) map[string]tmux.TmuxWindowInfo {
	m := make(map[string]tmux.TmuxWindowInfo)
	for _, w := range windows {
		// Key by session:window_name (matching how sessions store tmux_window)
		key := fmt.Sprintf("%s:%s", w.SessionName, w.WindowName)
		m[key] = w
		// Also key by window_id for alternate matching
		m[w.WindowID] = w
	}
	return m
}

// reconcileSession reconciles a single session.
func (r *Reconciler) reconcileSession(ctx context.Context, s store.SessionRow, windowMap map[string]tmux.TmuxWindowInfo) ReconcileResult {
	result := ReconcileResult{
		SessionID: s.ID,
		TopicID:   s.TopicID,
	}

	// Try to find matching tmux window
	key := fmt.Sprintf("%s:%s", s.TmuxSession, s.TmuxWindow)
	win, exists := windowMap[key]

	if !exists {
		// Window doesn't exist in tmux
		result.Action = "lost"
		result.Message = "❌ session lost, /resume to continue"

		if err := r.store.UpdateSessionStatus(s.ID, "failed"); err != nil {
			slog.Error("reconciler: failed to update status", "session_id", s.ID, "error", err)
		}

		// Notify topic
		if err := r.notifyTopic(ctx, s.TopicID, result.Message); err != nil {
			slog.Error("reconciler: failed to notify topic", "topic_id", s.TopicID, "error", err)
		}

		slog.Warn("reconciler: session lost (window not found)", "session_id", s.ID, "key", key)
		return result
	}

	// Window exists - check process liveness
	if win.PaneAlive && win.PanePID > 0 {
		// Process is alive - session recovered
		result.Action = "recovered"
		result.Message = "session recovered"

		if err := r.store.UpdateSessionStatus(s.ID, "active"); err != nil {
			slog.Error("reconciler: failed to update status to active", "session_id", s.ID, "error", err)
		}

		// Update PID if different
		if int64(win.PanePID) != s.PID.Int64 {
			if err := r.store.UpdateSessionPID(s.ID, win.PanePID); err != nil {
				slog.Error("reconciler: failed to update PID", "session_id", s.ID, "error", err)
			}
		}

		slog.Info("reconciler: session recovered", "session_id", s.ID, "pid", win.PanePID)
	} else {
		// Window exists but process is dead
		result.Action = "crashed"
		result.Message = "⚠️ session crashed, resuming..."

		if err := r.store.UpdateSessionStatus(s.ID, "crashed"); err != nil {
			slog.Error("reconciler: failed to update status to crashed", "session_id", s.ID, "error", err)
		}

		if err := r.notifyTopic(ctx, s.TopicID, result.Message); err != nil {
			slog.Error("reconciler: failed to notify topic", "topic_id", s.TopicID, "error", err)
		}

		slog.Warn("reconciler: session crashed (process dead)", "session_id", s.ID)
	}

	return result
}

// markAllFailed marks all sessions as failed when tmux server is unavailable.
func (r *Reconciler) markAllFailed(ctx context.Context, sessions []store.SessionRow) ([]ReconcileResult, error) {
	var results []ReconcileResult
	for _, s := range sessions {
		result := ReconcileResult{
			SessionID: s.ID,
			TopicID:   s.TopicID,
			Action:    "lost",
			Message:   "❌ session lost, /resume to continue",
		}

		if err := r.store.UpdateSessionStatus(s.ID, "failed"); err != nil {
			slog.Error("reconciler: failed to update status", "session_id", s.ID, "error", err)
		}

		if err := r.notifyTopic(ctx, s.TopicID, result.Message); err != nil {
			slog.Error("reconciler: failed to notify topic", "topic_id", s.TopicID, "error", err)
		}

		results = append(results, result)
	}
	return results, nil
}

// notifyTopic sends a notification to a Telegram topic.
func (r *Reconciler) notifyTopic(ctx context.Context, topicID int, message string) error {
	if r.sender == nil {
		slog.Warn("reconciler: no sender configured, skipping notification", "topic_id", topicID)
		return nil
	}

	topic, err := r.store.GetTopicByID(topicID)
	if err != nil {
		return fmt.Errorf("get topic %d: %w", topicID, err)
	}

	return r.sender.SendToTopic(topic.ChatID, topic.ThreadID, message)
}
