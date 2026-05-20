package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// editInterval is how often the thinking bubble is re-edited (spinner + elapsed).
// Kept >=2s to stay clear of Telegram's per-message edit flood limits.
const editInterval = 2 * time.Second

// staleThreshold: if the transcript hasn't grown for this long while a turn is
// in progress, the bubble switches to a "response delayed" warning so the user
// can tell Claude is stuck rather than thinking.
const staleThreshold = 20 * time.Second

// progressMaxWindow caps how long a bubble lives if never cleared (safety net).
const progressMaxWindow = 10 * time.Minute

// spinnerFrames animate the bubble so it visibly "moves".
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// TypingManager shows an animated "💭 생각 중 ⠹ 12s" bubble per topic while
// Claude works: a spinner that rotates and an elapsed-seconds counter, so the
// user can see the bot is alive. It also sends the header "typing…" action.
// When the transcript stops growing it flips to a "⚠️ 응답 지연" warning.
type TypingManager struct {
	client *Client
	logger *slog.Logger

	mu     sync.Mutex
	active map[string]*progress // "chatID:threadID" → state
}

type progress struct {
	chatID        int64
	threadID      int64
	messageID     int64 // the bubble message; 0 until first send succeeds
	started       time.Time
	deadline      time.Time
	lastHeartbeat time.Time
	frame         int
	lastText      string
}

// NewTypingManager creates a TypingManager.
func NewTypingManager(client *Client, logger *slog.Logger) *TypingManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &TypingManager{
		client: client,
		logger: logger,
		active: make(map[string]*progress),
	}
}

func typingKey(chatID, threadID int64) string {
	return fmt.Sprintf("%d:%d", chatID, threadID)
}

// Ping starts (or extends) the thinking bubble for a topic. Called when a user
// message is forwarded to Claude. Sends the initial bubble immediately so even
// fast responses show some animation.
func (m *TypingManager) Ping(chatID, threadID int64) {
	if m == nil {
		return
	}
	now := time.Now()
	key := typingKey(chatID, threadID)

	m.mu.Lock()
	p, ok := m.active[key]
	if !ok {
		p = &progress{chatID: chatID, threadID: threadID, started: now}
		m.active[key] = p
	}
	p.deadline = now.Add(progressMaxWindow)
	p.lastHeartbeat = now
	needSend := p.messageID == 0
	m.mu.Unlock()

	if needSend {
		go m.sendInitial(key, chatID, threadID)
	}
}

// Heartbeat records that the transcript grew (Claude is actively producing
// output). Resets the stale timer so the bubble keeps showing "생각 중".
func (m *TypingManager) Heartbeat(chatID, threadID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if p, ok := m.active[typingKey(chatID, threadID)]; ok {
		p.lastHeartbeat = time.Now()
	}
	m.mu.Unlock()
}

// Clear deletes the thinking bubble for a topic (response delivered).
func (m *TypingManager) Clear(chatID, threadID int64) {
	if m == nil {
		return
	}
	key := typingKey(chatID, threadID)
	m.mu.Lock()
	p, ok := m.active[key]
	delete(m.active, key)
	m.mu.Unlock()
	if ok && p.messageID != 0 {
		if err := m.client.DeleteMessage(context.Background(), chatID, p.messageID); err != nil {
			m.logger.Debug("delete thinking bubble failed", "chat_id", chatID, "thread_id", threadID, "error", err)
		}
	}
}

func (m *TypingManager) sendInitial(key string, chatID, threadID int64) {
	msg, err := m.client.SendMessage(context.Background(), chatID, "💭 생각 중 "+spinnerFrames[0]+" 0s", 0, threadID)
	if err != nil || msg == nil {
		m.logger.Debug("send thinking bubble failed", "chat_id", chatID, "thread_id", threadID, "error", err)
		return
	}
	m.mu.Lock()
	if p, ok := m.active[key]; ok {
		p.messageID = msg.MessageID
	} else {
		// Cleared before the send returned — delete the orphan bubble.
		go m.client.DeleteMessage(context.Background(), chatID, msg.MessageID)
	}
	m.mu.Unlock()
}

// Start runs the animation loop until ctx is cancelled.
func (m *TypingManager) Start(ctx context.Context) {
	ticker := time.NewTicker(editInterval)
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

func (m *TypingManager) tick(ctx context.Context) {
	now := time.Now()
	type job struct {
		chatID, threadID, messageID int64
		text                        string
	}
	var jobs []job
	var expired []*progress

	m.mu.Lock()
	for key, p := range m.active {
		if now.After(p.deadline) {
			expired = append(expired, p)
			delete(m.active, key)
			continue
		}
		if p.messageID == 0 {
			continue // initial send still in flight
		}
		elapsed := int(now.Sub(p.started).Seconds())
		var text string
		if now.Sub(p.lastHeartbeat) > staleThreshold {
			text = fmt.Sprintf("⚠️ 응답 지연… %ds (Claude 무응답)", elapsed)
		} else {
			p.frame = (p.frame + 1) % len(spinnerFrames)
			text = fmt.Sprintf("💭 생각 중 %s %ds", spinnerFrames[p.frame], elapsed)
		}
		if text == p.lastText {
			continue // no visible change, skip the edit (saves an API call)
		}
		p.lastText = text
		jobs = append(jobs, job{p.chatID, p.threadID, p.messageID, text})
		// header typing action too
		go m.client.SendChatAction(ctx, p.chatID, p.threadID, "typing")
	}
	m.mu.Unlock()

	for _, j := range jobs {
		if err := m.client.EditMessageText(ctx, j.chatID, j.messageID, j.text); err != nil {
			m.logger.Debug("edit thinking bubble failed", "chat_id", j.chatID, "error", err)
		}
	}
	for _, p := range expired {
		if p.messageID != 0 {
			go m.client.DeleteMessage(context.Background(), p.chatID, p.messageID)
		}
	}
}
