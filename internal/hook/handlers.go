package hook

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// ContextMonitor is the interface for context lifecycle monitoring.
type ContextMonitor interface {
	OnStopHook(ctx context.Context, sessionID, transcriptPath string) error
}

// Handlers processes different hook event types.
type Handlers struct {
	logger  *slog.Logger
	store   *store.Store
	sender  *bot.Sender
	monitor ContextMonitor
}

// NewHandlers creates new hook Handlers.
func NewHandlers(logger *slog.Logger, st *store.Store, sender *bot.Sender, monitor ContextMonitor) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{logger: logger, store: st, sender: sender, monitor: monitor}
}

// HandleSessionStart handles POST /hooks/session-start from Claude Code.
func (h *Handlers) HandleSessionStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Warn("hook session-start: invalid payload", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid payload"})
		return
	}

	sessionID, _ := payload["session_id"].(string)
	cwd, _ := payload["cwd"].(string)
	h.logger.Info("hook session-start received", "session_id", sessionID, "cwd", cwd)

	if cwd != "" && h.store != nil {
		sess, err := h.store.SessionByWorkspaceAndStatus(cwd, []string{"pending", "spawning"})
		if err != nil {
			h.logger.Warn("hook session-start: lookup session by workspace failed", "error", err, "cwd", cwd)
		} else if sess != nil {
			if err := h.store.UpdateSessionClaudeID(sess.ID, sessionID); err != nil {
				h.logger.Warn("hook session-start: update claude_session_id failed", "error", err)
			}
			if err := h.store.UpdateSessionStatus(sess.ID, "active"); err != nil {
				h.logger.Warn("hook session-start: update status to active failed", "error", err)
			}
			h.logger.Info("hook session-start: session activated", "tgcc_session_id", sess.ID, "claude_session_id", sessionID)
		} else {
			h.logger.Warn("hook session-start: no pending/spawning session found for cwd", "cwd", cwd)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// HandleStop handles POST /hooks/stop from Claude Code.
func (h *Handlers) HandleStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Warn("hook stop: invalid payload", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid payload"})
		return
	}

	sessionID, _ := payload["session_id"].(string)
	transcriptPath, _ := payload["transcript_path"].(string)
	h.logger.Info("hook stop received", "session_id", sessionID, "transcript_path", transcriptPath)

	if sessionID != "" && h.store != nil && h.sender != nil {
		// Look up by Claude session_id first (set during SessionStart hook),
		// fall back to cwd-based matching if the cwd field is available.
		sess, err := h.store.SessionByClaudeID(sessionID)
		if err != nil {
			h.logger.Warn("hook stop: session lookup by claude_id failed", "error", err, "session_id", sessionID)
		}
		if sess == nil {
			// Fallback: try to match by cwd (workspace path)
			if cwd, _ := payload["cwd"].(string); cwd != "" {
				sess, err = h.store.SessionByWorkspaceAndStatus(cwd, []string{"active", "idle", "compacting"})
				if err != nil {
					h.logger.Warn("hook stop: session lookup by workspace failed", "error", err, "cwd", cwd)
				}
			}
		}
		if sess != nil {
			topic, err := h.store.TopicByID(sess.TopicID)
			if err != nil {
				h.logger.Warn("hook stop: topic lookup failed", "error", err, "topic_id", sess.TopicID)
			} else if topic != nil {
				msg := h.extractLastAssistantMessage(transcriptPath)
				if msg != "" {
					h.sender.Enqueue(bot.OutgoingMsg{
						ChatID:   topic.ChatID,
						ThreadID: topic.ThreadID,
						Text:     msg,
					})
				}
			}
		}
	}

	// Also notify context monitor for context tracking/compaction
	if h.monitor != nil {
		if err := h.monitor.OnStopHook(context.Background(), sessionID, transcriptPath); err != nil {
			h.logger.Warn("context monitor OnStopHook failed", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// extractLastAssistantMessage reads a JSONL transcript and returns the content
// of the last assistant message. Returns empty string on any error.
func (h *Handlers) extractLastAssistantMessage(transcriptPath string) string {
	if transcriptPath == "" {
		return ""
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		h.logger.Warn("hook stop: open transcript failed", "path", transcriptPath, "error", err)
		return ""
	}
	defer f.Close()

	var lastAssistant string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var msg struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Role == "assistant" {
			lastAssistant = msg.Content
		}
	}
	_ = scanner.Err()
	if len(lastAssistant) > 4000 {
		lastAssistant = lastAssistant[:4000]
	}
	return lastAssistant
}

// HandleNotification handles POST /hooks/notification from Claude Code.
func (h *Handlers) HandleNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		h.logger.Warn("hook notification: invalid payload", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid payload"})
		return
	}

	sessionID, _ := payload["session_id"].(string)
	message, _ := payload["message"].(string)
	h.logger.Info("hook notification received", "session_id", sessionID, "message", message)

	if sessionID != "" && message != "" && h.store != nil && h.sender != nil {
		sess, err := h.store.SessionByID(sessionID)
		if err != nil {
			h.logger.Warn("hook notification: session lookup failed", "error", err, "session_id", sessionID)
		} else if sess != nil {
			topic, err := h.store.TopicByID(sess.TopicID)
			if err != nil {
				h.logger.Warn("hook notification: topic lookup failed", "error", err, "topic_id", sess.TopicID)
			} else if topic != nil {
				h.sender.Enqueue(bot.OutgoingMsg{
					ChatID:   topic.ChatID,
					ThreadID: topic.ThreadID,
					Text:     fmt.Sprintf("🔔 %s", message),
				})
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
