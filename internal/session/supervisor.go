package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/jaekwon-park/tgcc/internal/store"
)

const defaultSupervisorInterval = 30 * time.Second

// Supervisor periodically checks for crashed sessions and attempts to resume them.
type Supervisor struct {
	store    *store.Store
	mgr      *Manager
	interval time.Duration
	logger   *slog.Logger
}

// NewSupervisor creates a Supervisor with the given check interval.
// If interval is zero, defaultSupervisorInterval is used.
func NewSupervisor(st *store.Store, mgr *Manager, interval time.Duration) *Supervisor {
	if interval == 0 {
		interval = defaultSupervisorInterval
	}
	return &Supervisor{store: st, mgr: mgr, interval: interval, logger: slog.Default()}
}

// Start runs the supervisor loop until ctx is cancelled.
func (s *Supervisor) Start(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.recover(ctx)
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
		s.logger.Info("supervisor: resuming crashed session", "session_id", sess.ID)
		if err := s.store.UpdateSessionStatus(sess.ID, "resuming"); err != nil {
			s.logger.Error("supervisor: set resuming failed", "session_id", sess.ID, "err", err)
			continue
		}
		if _, err := s.mgr.Resume(ctx, sess.ID); err != nil {
			s.logger.Error("supervisor: resume failed", "session_id", sess.ID, "err", err)
			if uerr := s.store.UpdateSessionStatus(sess.ID, "failed"); uerr != nil {
				s.logger.Error("supervisor: set failed status error", "err", uerr)
			}
		}
	}
}
