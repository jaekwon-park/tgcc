package bot

import (
	"context"
	"log/slog"
	"time"
)

// OutgoingMsg represents a message to be sent to Telegram.
type OutgoingMsg struct {
	ChatID       int64
	ThreadID     int64
	Text         string
	ReplyToMsgID int64
}

// Sender queues outgoing messages and sends them respecting Telegram rate limits.
type Sender struct {
	client *Client
	logger *slog.Logger
	queue  chan OutgoingMsg
	done   chan struct{}
}

// NewSender creates a new Sender.
func NewSender(client *Client, logger *slog.Logger) *Sender {
	return &Sender{
		client: client,
		logger: logger,
		queue:  make(chan OutgoingMsg, 64),
		done:   make(chan struct{}),
	}
}

// Enqueue adds a message to the send queue (non-blocking, drops on full).
func (s *Sender) Enqueue(msg OutgoingMsg) {
	select {
	case s.queue <- msg:
	default:
		s.logger.Warn("dropping outgoing message: send queue full", "chat_id", msg.ChatID, "thread_id", msg.ThreadID)
	}
}

// Start begins consuming the send queue. Runs until ctx is cancelled.
func (s *Sender) Start(ctx context.Context) error {
	limiter := newRateLimiter(30, 30)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		case msg := <-s.queue:
			if err := limiter.wait(ctx); err != nil {
				return nil
			}
			if _, err := s.client.SendMessage(ctx, msg.ChatID, msg.Text, msg.ReplyToMsgID, msg.ThreadID); err != nil {
				s.logger.Error("sendMessage failed", "error", err, "chat_id", msg.ChatID, "thread_id", msg.ThreadID)
			}
		}
	}
}

// Stop signals the sender to shut down.
func (s *Sender) Stop() {
	close(s.done)
}

// rateLimiter is a simple token bucket for rate limiting.
type rateLimiter struct {
	capacity float64
	rate     float64 // tokens per second
	tokens   float64
	lastTime time.Time
}

// newRateLimiter creates a token bucket.
func newRateLimiter(ratePerSec int, burst int) *rateLimiter {
	return &rateLimiter{
		capacity: float64(burst),
		rate:     float64(ratePerSec),
		tokens:   float64(burst),
		lastTime: time.Now(),
	}
}

// allow returns true if a token is available, false otherwise.
func (rl *rateLimiter) allow() bool {
	now := time.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	if elapsed > 0 {
		rl.tokens += elapsed * rl.rate
		if rl.tokens > rl.capacity {
			rl.tokens = rl.capacity
		}
		rl.lastTime = now
	}
	if rl.tokens >= 1 {
		rl.tokens--
		return true
	}
	return false
}

// wait blocks until a token is available.
func (rl *rateLimiter) wait(ctx context.Context) error {
	for {
		if rl.allow() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
