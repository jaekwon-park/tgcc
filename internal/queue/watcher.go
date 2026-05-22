// Package queue monitors .notify-queue files in each topic workspace and
// injects a synthetic [queue-drain] message to wake up idle leader sessions.
package queue

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/jaekwon-park/tgcc/internal/session"
	"github.com/jaekwon-park/tgcc/internal/store"
)

const fileName = ".notify-queue"

// Watcher monitors .notify-queue files using fsnotify and wakes idle sessions.
type Watcher struct {
	store  *store.Store
	mgr    *session.Manager
	logger *slog.Logger

	watcher *fsnotify.Watcher

	mu             sync.Mutex
	debounceTimers map[string]*time.Timer

	debounceMs int64
	enabled    bool

	// watched tracks workspace paths already registered with fsnotify.
	watched map[string]bool
}

// NewWatcher creates a new queue Watcher.
// If enabled is false, Start() is a no-op.
func NewWatcher(st *store.Store, mgr *session.Manager, logger *slog.Logger, debounceMs int64, enabled bool) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	if debounceMs <= 0 {
		debounceMs = 200
	}
	return &Watcher{
		store:          st,
		mgr:            mgr,
		logger:         logger,
		debounceTimers: make(map[string]*time.Timer),
		debounceMs:     debounceMs,
		enabled:        enabled,
		watched:        make(map[string]bool),
	}
}

// Start sets up fsnotify watchers on all topic workspaces and begins the event loop.
// If the watcher is disabled, this is a no-op.
func (w *Watcher) Start(ctx context.Context) error {
	if !w.enabled {
		w.logger.Info("queue watcher disabled, skipping start")
		return nil
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("queue: create fsnotify watcher: %w", err)
	}
	w.watcher = fsw

	// Initial sync of workspace directories.
	w.syncWorkspaces(ctx)

	// Periodically resync workspace directories (new topics may be registered).
	resyncTicker := time.NewTicker(30 * time.Second)
	defer resyncTicker.Stop()

	w.logger.Info("queue watcher started", "debounce_ms", w.debounceMs)

	for {
		select {
		case <-ctx.Done():
			_ = w.Stop()
			w.logger.Info("queue watcher stopped")
			return nil
		case event, ok := <-fsw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ctx, event)
		case err, ok := <-fsw.Errors:
			if !ok {
				return nil
			}
			w.logger.Warn("queue: fsnotify error", "error", err)
		case <-resyncTicker.C:
			w.syncWorkspaces(ctx)
		}
	}
}

// Stop closes the fsnotify watcher and cancels all pending debounce timers.
func (w *Watcher) Stop() error {
	w.mu.Lock()
	for _, timer := range w.debounceTimers {
		timer.Stop()
	}
	w.debounceTimers = make(map[string]*time.Timer)
	w.mu.Unlock()

	if w.watcher != nil {
		return w.watcher.Close()
	}
	return nil
}

// handleEvent processes a single fsnotify event.
// Only Write and Create events on .notify-queue files are handled.
func (w *Watcher) handleEvent(ctx context.Context, event fsnotify.Event) {
	// Only care about files named .notify-queue.
	base := filepath.Base(event.Name)
	if base != fileName {
		return
	}

	// Only handle write and create events.
	if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
		return
	}

	workspacePath := filepath.Dir(event.Name)

	w.logger.Debug("queue: detected notify-queue change",
		"path", event.Name,
		"workspace", workspacePath,
		"op", event.Op.String(),
	)

	w.mu.Lock()
	// Cancel any existing debounce timer for this workspace.
	// If the timer already fired (Stop returns false), the callback is
	// running or about to run — we accept the double-fire as harmless
	// (processQueue is idempotent: it forwards [queue-drain] which
	// doesn't cause side effects if already in active state).
	if timer, ok := w.debounceTimers[workspacePath]; ok {
		timer.Stop()
	}
	// Schedule a new debounced processing.
	debounceDur := time.Duration(w.debounceMs) * time.Millisecond
	w.debounceTimers[workspacePath] = time.AfterFunc(debounceDur, func() {
		w.mu.Lock()
		delete(w.debounceTimers, workspacePath)
		w.mu.Unlock()
		w.processQueue(ctx, workspacePath)
	})
	w.mu.Unlock()
}

// processQueue checks whether the session for the given workspace is idle,
// and if so, injects the [queue-drain] trigger message.
func (w *Watcher) processQueue(ctx context.Context, workspacePath string) {
	// Guard against execution after Stop() has been called — ctx is cancelled
	// when the watcher is shutting down so debounce callbacks won't operate on
	// a closed fsnotify watcher or stopped session manager.
	if ctx.Err() != nil {
		return
	}
	// Look for an idle session at this workspace.
	sess, err := w.store.SessionByWorkspaceAndStatus(workspacePath, []string{string(session.StatusIdle)})
	if err != nil {
		w.logger.Warn("queue: lookup session by workspace failed",
			"workspace", workspacePath,
			"error", err,
		)
		return
	}
	if sess == nil {
		w.logger.Debug("queue: no idle session for workspace, skipping",
			"workspace", workspacePath,
		)
		return
	}

	w.logger.Info("queue: waking idle session",
		"session_id", sess.ID,
		"workspace", workspacePath,
	)

	if err := w.mgr.ForwardMessage(ctx, sess.ID, "[queue-drain]"); err != nil {
		w.logger.Error("queue: forward [queue-drain] failed",
			"session_id", sess.ID,
			"workspace", workspacePath,
			"error", err,
		)
		return
	}

	w.logger.Info("queue: [queue-drain] injected",
		"session_id", sess.ID,
		"workspace", workspacePath,
	)
}

// addWorkspace adds a workspace directory to the fsnotify watcher.
// Skips directories that don't exist or are already watched.
func (w *Watcher) addWorkspace(workspacePath string) {
	workspacePath = filepath.Clean(workspacePath)
	if workspacePath == "" {
		return
	}

	w.mu.Lock()
	if w.watched[workspacePath] {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	// Only watch if the directory exists (may not have .notify-queue yet).
	info, err := os.Stat(workspacePath)
	if err != nil || !info.IsDir() {
		w.logger.Debug("queue: workspace does not exist, skipping add",
			"workspace", workspacePath,
		)
		return
	}

	// Watch the workspace directory itself — fsnotify will report events for
	// files within it, including .notify-queue.
	if err := w.watcher.Add(workspacePath); err != nil {
		w.logger.Warn("queue: failed to add workspace watch",
			"workspace", workspacePath,
			"error", err,
		)
		// Do NOT mark as watched — allows retry on next syncWorkspaces.
		return
	}

	w.mu.Lock()
	w.watched[workspacePath] = true
	w.mu.Unlock()

	w.logger.Debug("queue: watching workspace", "workspace", workspacePath)
}

// syncWorkspaces refreshes the list of watched directories from the store.
// Called periodically to pick up newly registered topics.
func (w *Watcher) syncWorkspaces(ctx context.Context) {
	topics, err := w.store.TopicsWithWorkspace()
	if err != nil {
		w.logger.Warn("queue: list topics failed", "error", err)
		return
	}
	for _, topic := range topics {
		if topic.WorkspacePath == "" {
			continue
		}
		// Also remove the .notify-queue suffix if someone accidentally uses the file path.
		wp := topic.WorkspacePath
		if strings.HasSuffix(wp, "/"+fileName) {
			wp = wp[:len(wp)-len(fileName)-1]
		}
		w.addWorkspace(wp)
	}
	_ = ctx
}
