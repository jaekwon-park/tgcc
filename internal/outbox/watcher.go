// Package outbox delivers files an agent drops in its workspace outbox to the
// bound Telegram topic. An agent writes a file to <workspace>/outbox/ and the
// watcher sends it as a document to that topic's chat/thread, then deletes it
// after a TTL.
package outbox

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// dirName is the per-workspace subdirectory agents write files into.
const dirName = "outbox"

// pollInterval is how often the watcher scans every workspace outbox.
const pollInterval = 2 * time.Second

// Watcher scans each topic's <workspace>/outbox/ directory and delivers files.
type Watcher struct {
	store  *store.Store
	client *bot.Client
	ttl    time.Duration
	logger *slog.Logger

	// sent tracks files already delivered: path -> delivery time. A file stays
	// here until the TTL elapses, then it is deleted and the entry removed.
	sent map[string]time.Time
}

// NewWatcher creates an outbox Watcher.
func NewWatcher(st *store.Store, client *bot.Client, cfg config.OutboxConfig, logger *slog.Logger) *Watcher {
	if logger == nil {
		logger = slog.Default()
	}
	ttl := time.Duration(cfg.TTLSec) * time.Second
	if ttl <= 0 {
		ttl = time.Duration(config.DefaultOutboxConfig().TTLSec) * time.Second
	}
	return &Watcher{
		store:  st,
		client: client,
		ttl:    ttl,
		logger: logger,
		sent:   make(map[string]time.Time),
	}
}

// Start runs the scan/cleanup loop until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context) {
	w.logger.Info("outbox watcher started", "ttl", w.ttl)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("outbox watcher stopped")
			return
		case <-ticker.C:
			w.scan(ctx)
			w.cleanup()
		}
	}
}

// scan walks every topic's outbox directory and delivers new files.
func (w *Watcher) scan(ctx context.Context) {
	topics, err := w.store.TopicsWithWorkspace()
	if err != nil {
		w.logger.Error("outbox: list topics failed", "error", err)
		return
	}
	for _, topic := range topics {
		dir := filepath.Join(topic.WorkspacePath, dirName)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				w.logger.Warn("outbox: read dir failed", "dir", dir, "error", err)
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if _, seen := w.sent[path]; seen {
				continue
			}
			w.deliver(ctx, topic, path)
		}
	}
}

// deliver sends a single file to its topic. The file is marked as sent
// regardless of outcome so a failed send isn't retried in a tight loop; the
// cleanup pass deletes it once the TTL elapses.
func (w *Watcher) deliver(ctx context.Context, topic *store.Topic, path string) {
	if _, err := w.client.SendDocument(ctx, topic.ChatID, topic.ThreadID, path); err != nil {
		w.logger.Error("outbox: send failed", "path", path, "chat_id", topic.ChatID, "thread_id", topic.ThreadID, "error", err)
	} else {
		w.logger.Info("outbox: delivered", "path", path, "chat_id", topic.ChatID, "thread_id", topic.ThreadID)
	}
	w.sent[path] = time.Now()
}

// cleanup deletes delivered files whose TTL has elapsed.
func (w *Watcher) cleanup() {
	now := time.Now()
	for path, sentAt := range w.sent {
		if now.Sub(sentAt) < w.ttl {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			w.logger.Warn("outbox: delete failed", "path", path, "error", err)
		}
		delete(w.sent, path)
	}
}
