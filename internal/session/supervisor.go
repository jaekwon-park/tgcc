package session

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	"github.com/jaekwon-park/tgcc/internal/honcho"
	"github.com/jaekwon-park/tgcc/internal/store"
)

const defaultSupervisorInterval = 30 * time.Second

// hibernateSmallTranscriptThreshold is the minimum transcript size to hibernate.
const hibernateSmallTranscriptThreshold int64 = 50000

type Supervisor struct {
	store        *store.Store
	mgr          *Manager
	interval     time.Duration
	logger       *slog.Logger
	cfg          config.ContextConfig
	sender       *bot.Sender
	honchoClient *honcho.HonchoClient
}

func NewSupervisor(st *store.Store, mgr *Manager, interval time.Duration, cfg config.ContextConfig, sender *bot.Sender, honchoClient *honcho.HonchoClient) *Supervisor {
	if interval == 0 {
		interval = defaultSupervisorInterval
	}
	return &Supervisor{
		store:        st,
		mgr:          mgr,
		interval:     interval,
		logger:       slog.Default(),
		cfg:          cfg,
		sender:       sender,
		honchoClient: honchoClient,
	}
}

func (s *Supervisor) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recover(ctx)
			s.hibernate(ctx)
		}
	}
}

func (s *Supervisor) recover(ctx context.Context) {
	sessions, err := s.store.ActiveSessions([]string{"crashed"})
	if err != nil {
		s.logger.Error("supervisor: query crashed sessions", "err", err)
		return
	}

	for _, sess := range sessions {
		if sess.TranscriptBytes < s.cfg.FreshRestartBytes && sess.TurnCount < 100 {
			s.logger.Info("supervisor: resuming crashed session", "session_id", sess.ID)
			if err := s.store.UpdateSessionStatus(sess.ID, "resuming"); err != nil {
				s.logger.Error("supervisor: set resuming failed", "session_id", sess.ID, "err", err)
				continue
			}
			if _, err := s.mgr.Resume(ctx, sess.ID); err != nil {
				s.logger.Error("supervisor: resume failed", "session_id", sess.ID, "err", err)
				if uerr := s.store.UpdateSessionStatus(sess.ID, "failed"); uerr != nil {
					s.logger.Error("supervisor: set failed status error", "session_id", sess.ID, "err", uerr)
				}
			}
			continue
		}

		s.logger.Info(
			"supervisor: fresh restart for crashed session",
			"session_id", sess.ID,
			"transcript_bytes", sess.TranscriptBytes,
			"turn_count", sess.TurnCount,
		)

		summary := ""
		if sess.TranscriptPath != "" {
			summary, err = s.mgr.SummarizeLastNTurns(sess.TranscriptPath, 10)
			if err != nil {
				s.logger.Warn("supervisor: summarize failed, continuing with empty summary", "session_id", sess.ID, "err", err)
				summary = ""
			}
		}

		// Enrich summary with Honcho long-term memory (nil-safe: no-op when disabled).
		honchoSessionID := fmt.Sprintf("tgcc-topic-%d", sess.TopicID)
		summary = s.honchoClient.BuildResumeContext(ctx, honchoSessionID, summary)

		var chatID int64
		var threadID int64
		topic, terr := s.store.TopicByID(sess.TopicID)
		if terr != nil {
			s.logger.Warn("supervisor: topic lookup failed", "session_id", sess.ID, "topic_id", sess.TopicID, "err", terr)
		} else if topic != nil {
			chatID = topic.ChatID
			threadID = topic.ThreadID
		}

		if _, err := s.mgr.FreshRestart(ctx, sess.ID, summary, chatID, threadID); err != nil {
			s.logger.Error("supervisor: fresh restart failed", "session_id", sess.ID, "err", err)
			if uerr := s.store.UpdateSessionStatus(sess.ID, "failed"); uerr != nil {
				s.logger.Error("supervisor: set failed status error", "session_id", sess.ID, "err", uerr)
			}
		}
	}
}

func (s *Supervisor) hibernate(ctx context.Context) {
	_ = ctx

	idleMs := s.cfg.IdleHibernateMin * 60 * 1000
	if idleMs <= 0 {
		return
	}

	sessions, err := s.store.ActiveSessions([]string{"active", "idle"})
	if err != nil {
		s.logger.Error("supervisor: query active/idle sessions", "err", err)
		return
	}

	nowMs := store.CurrentTimeMs()
	for _, sess := range sessions {
		if nowMs-sess.LastActivityAt <= idleMs {
			continue
		}
		if sess.TranscriptBytes < hibernateSmallTranscriptThreshold {
			s.logger.Debug(
				"supervisor: skip hibernate for small transcript",
				"session_id", sess.ID,
				"transcript_bytes", sess.TranscriptBytes,
			)
			continue
		}

		if err := s.store.UpdateSessionStatus(sess.ID, string(StatusHibernated)); err != nil {
			s.logger.Error("supervisor: set hibernated failed", "session_id", sess.ID, "err", err)
			continue
		}

		target := sess.TmuxWindow
		if target == "" {
			target = fmt.Sprintf("%s:%s", sess.TmuxSession, sess.TmuxWindow)
		}
		if err := s.mgr.KillWindow(target); err != nil {
			s.logger.Warn("supervisor: kill-window on hibernate failed", "session_id", sess.ID, "target", target, "err", err)
		}

		topic, terr := s.store.TopicByID(sess.TopicID)
		if terr != nil {
			s.logger.Warn("supervisor: topic lookup failed on hibernate", "session_id", sess.ID, "topic_id", sess.TopicID, "err", terr)
			continue
		}
		if topic == nil || s.sender == nil {
			continue
		}

		s.sender.Enqueue(bot.OutgoingMsg{
			ChatID:   topic.ChatID,
			ThreadID: topic.ThreadID,
			Text:     "💤 30분 무활동 — 세션 정리됨. 메시지 보내면 재시작",
		})
	}
}
