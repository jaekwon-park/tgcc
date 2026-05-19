package session

import (
	"context"
	"log/slog"

	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

// Reconciler synchronises the SQLite sessions table with actual tmux state at startup.
type Reconciler struct {
	store  *store.Store
	tmux   *tmux.Adapter
	logger *slog.Logger
}

// NewReconciler creates a Reconciler.
func NewReconciler(st *store.Store, ta *tmux.Adapter) *Reconciler {
	return &Reconciler{store: st, tmux: ta, logger: slog.Default()}
}

// Run performs a one-shot reconciliation: sessions that appear alive in DB but
// have no corresponding tmux window are marked as crashed.
func (r *Reconciler) Run(ctx context.Context, tmuxSession string) error {
	sessions, err := r.store.ActiveSessions([]string{"active", "idle", "resuming"})
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		return nil
	}

	windows, err := r.tmux.ListWindows(tmuxSession)
	if err != nil {
		r.logger.Warn("reconciler: cannot list tmux windows", "err", err)
		return nil
	}

	alive := make(map[string]bool, len(windows))
	for _, w := range windows {
		alive[w.ID] = true
	}

	for _, s := range sessions {
		if alive[s.TmuxWindow] {
			continue
		}
		r.logger.Info("reconciler: session window gone, marking crashed",
			"session_id", s.ID, "window", s.TmuxWindow)
		if err := r.store.UpdateSessionStatus(s.ID, "crashed"); err != nil {
			r.logger.Error("reconciler: update status failed", "session_id", s.ID, "err", err)
		}
	}
	return nil
}
