package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

// Supervisor monitors active/idle sessions and handles crash recovery.
type Supervisor struct {
	store      *store.Store
	tmux       *tmux.Adapter
	sender     NotificationSender
	maxRetries int
	tick       time.Duration

	// Track retry counts per session
	retryCounts map[string]int
}

// SupervisorConfig configures a Supervisor.
type SupervisorConfig struct {
	Store      *store.Store
	Tmux       *tmux.Adapter
	Sender     NotificationSender
	MaxRetries int           // default 3
	Tick       time.Duration // default 5s
}

// NewSupervisor creates a new Supervisor.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 3
	}
	if cfg.Tick == 0 {
		cfg.Tick = 5 * time.Second
	}
	return &Supervisor{
		store:       cfg.Store,
		tmux:        cfg.Tmux,
		sender:      cfg.Sender,
		maxRetries:  cfg.MaxRetries,
		tick:        cfg.Tick,
		retryCounts: make(map[string]int),
	}
}

// Run starts the supervisor loop. It blocks until ctx is cancelled.
//
// Every tick (default 5s):
//  1. Query active/idle sessions from SQLite
//  2. Check each session's tmux pane liveness
//  3. On crash:
//     - Mark status=crashed
//     - Send "⚠️ session crashed, restarting..." notification
//     - Attempt restart with exponential backoff (1s → 2s → 4s, max 3 attempts)
//     - On success: status=active, "✅ resumed"
//     - On 3 failures: status=failed, "❌ resume failed, /new 필요"
//
// TODO: Phase 3 implementation - delegate to sub-model
func (s *Supervisor) Run(ctx context.Context) error {
	slog.Info("supervisor: starting", "tick", s.tick, "max_retries", s.maxRetries)

	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("supervisor: shutting down")
			return ctx.Err()
		case <-ticker.C:
			s.checkSessions(ctx)
		}
	}
}

// checkSessions performs one round of health checks on all active/idle sessions.
func (s *Supervisor) checkSessions(ctx context.Context) {
	sessions, err := s.store.GetSupervisedSessions()
	if err != nil {
		slog.Error("supervisor: failed to query sessions", "error", err)
		return
	}

	if len(sessions) == 0 {
		return
	}

	for _, session := range sessions {
		select {
		case <-ctx.Done():
			return
		default:
		}

		alive, _, err := s.tmux.IsWindowAlive(ctx, session.TmuxSession, session.TmuxWindow)
		if err != nil {
			slog.Warn("supervisor: window check failed", "session_id", session.ID, "error", err)
			continue
		}

		if alive {
			// Session is healthy - reset retry count
			delete(s.retryCounts, session.ID)
			continue
		}

		// Session crashed - handle recovery
		s.handleCrash(ctx, session)
	}
}

// handleCrash handles a crashed session with exponential backoff retry.
func (s *Supervisor) handleCrash(ctx context.Context, session store.SessionRow) {
	retries := s.retryCounts[session.ID]

	slog.Warn("supervisor: session crashed",
		"session_id", session.ID,
		"topic_id", session.TopicID,
		"retries", retries,
		"max_retries", s.maxRetries,
	)

	// Mark as crashed
	if err := s.store.UpdateSessionStatus(session.ID, "crashed"); err != nil {
		slog.Error("supervisor: failed to mark crashed", "session_id", session.ID, "error", err)
	}

	// Send crash notification
	s.notifyTopic(ctx, session.TopicID, "⚠️ session crashed, restarting...")

	if retries >= s.maxRetries {
		// Max retries exceeded - mark as failed
		if err := s.store.UpdateSessionStatus(session.ID, "failed"); err != nil {
			slog.Error("supervisor: failed to mark failed", "session_id", session.ID, "error", err)
		}

		s.notifyTopic(ctx, session.TopicID, "❌ resume failed, /new 필요")
		delete(s.retryCounts, session.ID)
		return
	}

	// Calculate backoff: 1s, 2s, 4s for retries 0, 1, 2
	backoff := time.Duration(1<<uint(retries)) * time.Second
	slog.Info("supervisor: backing off before retry",
		"session_id", session.ID,
		"backoff", backoff,
		"attempt", retries+1,
	)

	select {
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}

	// Update status to resuming
	if err := s.store.UpdateSessionStatus(session.ID, "resuming"); err != nil {
		slog.Error("supervisor: failed to mark resuming", "session_id", session.ID, "error", err)
	}

	// Attempt resume
	if err := s.attemptResume(ctx, session); err != nil {
		slog.Error("supervisor: resume attempt failed",
			"session_id", session.ID,
			"attempt", retries+1,
			"error", err,
		)
		s.retryCounts[session.ID] = retries + 1
		return
	}

	// Resume successful
	if err := s.store.UpdateSessionStatus(session.ID, "active"); err != nil {
		slog.Error("supervisor: failed to mark active after resume", "session_id", session.ID, "error", err)
	}

	s.notifyTopic(ctx, session.TopicID, "✅ resumed")
	delete(s.retryCounts, session.ID)
	slog.Info("supervisor: session resumed successfully", "session_id", session.ID)
}

// attemptResume tries to restart a crashed session.
func (s *Supervisor) attemptResume(ctx context.Context, session store.SessionRow) error {
	// Kill the old window if it still exists
	_ = s.tmux.KillWindow(ctx, session.TmuxSession, session.TmuxWindow)

	// Build resume command
	cmd := s.buildResumeCommand(session)

	// Create new tmux window
	return s.tmux.NewWindow(ctx, session.TmuxSession, session.TmuxWindow, session.WorkspacePath, cmd)
}

// buildResumeCommand builds the command to resume a Claude session.
func (s *Supervisor) buildResumeCommand(session store.SessionRow) string {
	if session.ClaudeSessionID.Valid && session.ClaudeSessionID.String != "" {
		return fmt.Sprintf("claude --resume %s", session.ClaudeSessionID.String)
	}
	// Fallback: start new session in the workspace
	return "claude"
}

// notifyTopic sends a notification to a Telegram topic.
func (s *Supervisor) notifyTopic(ctx context.Context, topicID int, message string) {
	if s.sender == nil {
		slog.Warn("supervisor: no sender configured, skipping notification", "topic_id", topicID)
		return
	}

	topic, err := s.store.GetTopicByID(topicID)
	if err != nil {
		slog.Error("supervisor: get topic failed", "topic_id", topicID, "error", err)
		return
	}

	if err := s.sender.SendToTopic(topic.ChatID, topic.ThreadID, message); err != nil {
		slog.Error("supervisor: send notification failed",
			"topic_id", topicID,
			"chat_id", topic.ChatID,
			"thread_id", topic.ThreadID,
			"error", err,
		)
	}
}

// ResetRetries resets the retry count for a session.
func (s *Supervisor) ResetRetries(sessionID string) {
	delete(s.retryCounts, sessionID)
}
