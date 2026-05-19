package bot

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Listener polls Telegram getUpdates and dispatches Update events to a channel.
type Listener struct {
	client   *Client
	logger   *slog.Logger
	updates  chan Update
	done     chan struct{}
	stopOnce sync.Once // L2 fix: prevents double-close panic
}

// NewListener creates a new Listener.
func NewListener(client *Client, logger *slog.Logger) *Listener {
	return &Listener{
		client:  client,
		logger:  logger,
		updates: make(chan Update, 16),
		done:    make(chan struct{}),
	}
}

// Updates returns the channel that receives Update events.
func (l *Listener) Updates() <-chan Update {
	return l.updates
}

// Start begins the long-polling loop. Runs until ctx is cancelled.
func (l *Listener) Start(ctx context.Context) error {
	var offset int64
	backoff := time.Second

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-l.done:
			return nil
		default:
		}

		updates, err := l.client.GetUpdates(ctx, offset, 30)
		if err != nil {
			l.logger.Error("getUpdates failed", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-l.done:
				return nil
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
			continue
		}

		backoff = time.Second
		for _, update := range updates {
			select {
			case l.updates <- update:
			default:
				l.logger.Warn("dropping update: updates channel full", "update_id", update.UpdateID)
			}
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
		}
	}
}

// Stop signals the listener to shut down.
// L2 fix: use sync.Once to prevent panic on double close.
func (l *Listener) Stop() {
	l.stopOnce.Do(func() {
		close(l.done)
	})
}
