package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// typingRefresh is how often the manager re-sends the "typing…" chat action
// while a topic is active. Telegram expires the indicator after ~5s, so this
// must be shorter than that.
const typingRefresh = 4 * time.Second

// typingMaxWindow caps how long a single user message keeps the indicator
// alive if it is never explicitly cleared (e.g. response relay missed). Safety
// net so the bot doesn't appear to type forever.
const typingMaxWindow = 5 * time.Minute

// TypingManager shows a live "typing…" indicator in topics where the bot is
// working, replacing a static "처리 중" message. Ping() starts/extends it for a
// topic; Clear() stops it (called when the response is relayed).
type TypingManager struct {
	client *Client
	logger *slog.Logger

	mu       sync.Mutex
	deadline map[string]time.Time // "chatID:threadID" → stop showing after this time
}

// NewTypingManager creates a TypingManager.
func NewTypingManager(client *Client, logger *slog.Logger) *TypingManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &TypingManager{
		client:   client,
		logger:   logger,
		deadline: make(map[string]time.Time),
	}
}

func typingKey(chatID, threadID int64) string {
	return fmt.Sprintf("%d:%d", chatID, threadID)
}

// Ping marks a topic as actively working: the indicator shows on the next loop
// tick and keeps refreshing until Clear() or typingMaxWindow elapses.
func (m *TypingManager) Ping(chatID, threadID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.deadline[typingKey(chatID, threadID)] = time.Now().Add(typingMaxWindow)
	m.mu.Unlock()
}

// Clear stops the indicator for a topic (response delivered).
func (m *TypingManager) Clear(chatID, threadID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.deadline, typingKey(chatID, threadID))
	m.mu.Unlock()
}

// Start runs the refresh loop until ctx is cancelled.
func (m *TypingManager) Start(ctx context.Context) {
	ticker := time.NewTicker(typingRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

// tick sends "typing…" to every topic still within its window and prunes
// expired entries.
func (m *TypingManager) tick(ctx context.Context) {
	now := time.Now()
	type target struct{ chatID, threadID int64 }
	var targets []target

	m.mu.Lock()
	for key, dl := range m.deadline {
		if now.After(dl) {
			delete(m.deadline, key)
			continue
		}
		var c, t int64
		fmt.Sscanf(key, "%d:%d", &c, &t)
		targets = append(targets, target{c, t})
	}
	m.mu.Unlock()

	for _, tg := range targets {
		if err := m.client.SendChatAction(ctx, tg.chatID, tg.threadID, "typing"); err != nil {
			m.logger.Debug("typing action failed", "chat_id", tg.chatID, "thread_id", tg.threadID, "error", err)
		}
	}
}
