// Package context monitors Claude Code session context usage and triggers compaction.
package context

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

const (
	compactCooldown = 5 * time.Minute
	maxCompacts     = 3
)

// Monitor tracks context size per session and triggers compaction when thresholds are crossed.
type Monitor struct {
	store  *store.Store
	tmux   *tmux.Adapter
	sender *bot.Sender
	cfg    config.ContextConfig
	logger *slog.Logger
	warned map[string]bool
	mu     sync.Mutex
}

// NewMonitor creates a new Monitor.
func NewMonitor(st *store.Store, tx *tmux.Adapter, sender *bot.Sender, cfg config.ContextConfig, logger *slog.Logger) *Monitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Monitor{
		store:  st,
		tmux:   tx,
		sender: sender,
		cfg:    cfg,
		logger: logger,
		warned: make(map[string]bool),
	}
}

// OnStopHook is called by the Claude Code stop hook after each turn.
// It updates context stats and checks thresholds.
func (m *Monitor) OnStopHook(ctx context.Context, sessionID, transcriptPath string, chatID, threadID int64) error {
	session, err := m.store.SessionByID(sessionID)
	if err != nil {
		return fmt.Errorf("look up session %s: %w", sessionID, err)
	}
	if session == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}

	fi, err := os.Stat(transcriptPath)
	if err != nil {
		return fmt.Errorf("stat transcript %s: %w", transcriptPath, err)
	}
	transcriptBytes := fi.Size()
	turnCount := session.TurnCount + 1

	if err := m.store.UpdateSessionContext(sessionID, transcriptBytes, turnCount); err != nil {
		return fmt.Errorf("update context stats: %w", err)
	}
	session.TranscriptBytes = transcriptBytes
	session.TurnCount = turnCount

	// Save transcript path for crash recovery / refresh
	if err := m.store.UpdateSessionTranscriptPath(sessionID, transcriptPath); err != nil {
		m.logger.Warn("update transcript path failed", "error", err)
	}

	// If we're in compacting state, the compact just finished — re-stat and restore active.
	if session.Status == "compacting" {
		fi2, err := os.Stat(transcriptPath)
		if err == nil {
			session.TranscriptBytes = fi2.Size()
			if err := m.store.UpdateSessionContext(sessionID, fi2.Size(), turnCount); err != nil {
				m.logger.Warn("update context after compact", "error", err)
			}
		}
		if err := m.store.UpdateSessionCompact(sessionID, session.CompactCount+1, store.CurrentTimeMs()); err != nil {
			m.logger.Warn("record compact", "error", err)
		}
		if err := m.store.UpdateSessionStatus(sessionID, "active"); err != nil {
			m.logger.Warn("restore active after compact", "error", err)
		}
		return nil
	}

	m.checkThresholds(ctx, session, chatID, threadID)
	return nil
}

// checkThresholds inspects byte and turn counts and triggers soft warn or hard compact.
func (m *Monitor) checkThresholds(ctx context.Context, session *store.Session, chatID, threadID int64) {
	bytes := session.TranscriptBytes
	turns := session.TurnCount

	softWarnHit := bytes >= m.cfg.SoftWarnBytes || turns >= m.cfg.SoftWarnTurns
	hardCompactHit := bytes >= m.cfg.HardCompactBytes || turns >= m.cfg.HardCompactTurns

	if hardCompactHit {
		// Safety: cooldown check.
		if session.LastCompactAt > 0 {
			lastCompact := time.UnixMilli(session.LastCompactAt)
			if time.Since(lastCompact) < compactCooldown {
				m.logger.Info("skipping compact: cooldown active", "session_id", session.ID)
				return
			}
		}
		// Safety: max compacts.
		if session.CompactCount >= maxCompacts {
			m.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chatID,
				ThreadID: threadID,
				Text:     fmt.Sprintf("⚠️ 세션 컨텍스트 한계 도달 (compact %d회 완료). 수동 처리가 필요합니다.", session.CompactCount),
			})
			return
		}

		if err := m.CompactSession(ctx, session.ID, chatID, threadID); err != nil {
			m.logger.Error("auto compact failed", "session_id", session.ID, "error", err)
		}
		return
	}

	if softWarnHit {
		m.mu.Lock()
		alreadyWarned := m.warned[session.ID]
		if !alreadyWarned {
			m.warned[session.ID] = true
		}
		m.mu.Unlock()

		if !alreadyWarned {
			m.sender.Enqueue(bot.OutgoingMsg{
				ChatID:   chatID,
				ThreadID: threadID,
				Text: fmt.Sprintf(
					"⚠️ 컨텍스트 경고: %s / %s (soft warn 기준 초과)",
					formatBytes(bytes), formatBytes(m.cfg.SoftWarnBytes),
				),
			})
		}
	}
}

// CompactSession sends /compact to the session via tmux and notifies Telegram.
func (m *Monitor) CompactSession(ctx context.Context, sessionID string, chatID, threadID int64) error {
	session, err := m.store.SessionByID(sessionID)
	if err != nil {
		return fmt.Errorf("look up session: %w", err)
	}
	if session == nil {
		return fmt.Errorf("session %s not found", sessionID)
	}

	target := session.TmuxWindow
	if target == "" {
		target = session.TmuxSession
	}

	if err := m.store.UpdateSessionStatus(sessionID, "compacting"); err != nil {
		return fmt.Errorf("set compacting status: %w", err)
	}
	if err := m.store.UpdateSessionCompact(sessionID, session.CompactCount, store.CurrentTimeMs()); err != nil {
		m.logger.Warn("record compact timestamp", "error", err)
	}

	if err := m.tmux.SendKeys(target, "/compact"); err != nil {
		_ = m.store.UpdateSessionStatus(sessionID, "active")
		return fmt.Errorf("send /compact: %w", err)
	}

	m.sender.Enqueue(bot.OutgoingMsg{
		ChatID:   chatID,
		ThreadID: threadID,
		Text:     fmt.Sprintf("🗜️ 컨텍스트 자동 compact 시작 (크기: %s, 턴: %d)", formatBytes(session.TranscriptBytes), session.TurnCount),
	})

	m.mu.Lock()
	delete(m.warned, sessionID)
	m.mu.Unlock()

	return nil
}

// CtxStatus returns a Korean-formatted context status string for the given session.
// Uses global config thresholds. For topic-specific thresholds, use CtxStatusWithOverrides.
func (m *Monitor) CtxStatus(ctx context.Context, sessionID string) string {
	return m.CtxStatusWithOverrides(ctx, sessionID, nil)
}

// CtxStatusWithOverrides returns a context status string using topic-level overrides
// when provided. Overrides are merged on top of global config.
func (m *Monitor) CtxStatusWithOverrides(ctx context.Context, sessionID string, overrides *ContextOverrides) string {
	session, err := m.store.SessionByID(sessionID)
	if err != nil || session == nil {
		return "세션을 찾을 수 없습니다."
	}

	cfg := MergeWithGlobal(overrides, m.cfg)

	softOK := session.TranscriptBytes < cfg.SoftWarnBytes && session.TurnCount < cfg.SoftWarnTurns
	softIcon := "✅"
	softLabel := "통과"
	if !softOK {
		softIcon = "⚠️"
		softLabel = "초과"
	}

	hardCompactKB := cfg.HardCompactBytes / 1024
	softWarnKB := cfg.SoftWarnBytes / 1024
	sizeKB := session.TranscriptBytes / 1024

	overrideNote := ""
	if overrides != nil {
		overrideNote = "\n⚠️ 토픽별 오버라이드 적용 중"
	}

	return fmt.Sprintf(
		"📊 컨텍스트 상태\n턴 수: %d / %d (자동 compact)\n크기: %d KB / %d KB (자동 compact)\n경고선: %s soft warn %s (%d KB)\ncompact 횟수: %d%s",
		session.TurnCount, cfg.HardCompactTurns,
		sizeKB, hardCompactKB,
		softIcon, softLabel, softWarnKB,
		session.CompactCount,
		overrideNote,
	)
}

// ResetWarned clears the soft-warn flag for a session (used in tests).
func (m *Monitor) ResetWarned(sessionID string) {
	m.mu.Lock()
	delete(m.warned, sessionID)
	m.mu.Unlock()
}

// formatBytes returns a human-readable byte count string.
func formatBytes(b int64) string {
	if b >= 1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1024*1024))
	}
	if b >= 1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/float64(1024))
	}
	return fmt.Sprintf("%d B", b)
}
