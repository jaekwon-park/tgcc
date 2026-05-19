package hook

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

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

	// Try correlation_id matching first (precise 1:1 match, prevents race on same workspace)
	correlationID := ""
	if env, ok := payload["env"].(map[string]interface{}); ok {
		if cid, ok2 := env["TGCC_CORRELATION_ID"].(string); ok2 {
			correlationID = cid
		}
	}

	var sess *store.Session
	if correlationID != "" && h.store != nil {
		var lookupErr error
		sess, lookupErr = h.store.SessionByCorrelationID(correlationID)
		if lookupErr != nil {
			h.logger.Warn("hook session-start: lookup by correlation_id failed", "error", lookupErr, "correlation_id", correlationID)
		}
	}

	// Fall back to cwd matching if correlation_id didn't find a match
	if sess == nil && cwd != "" && h.store != nil {
		var lookupErr error
		sess, lookupErr = h.store.SessionByWorkspaceAndStatus(cwd, []string{"pending", "spawning"})
		if lookupErr != nil {
			h.logger.Warn("hook session-start: lookup session by workspace failed", "error", lookupErr, "cwd", cwd)
		}
	}

	if sess != nil {
		if err := h.store.UpdateSessionClaudeID(sess.ID, sessionID); err != nil {
			h.logger.Warn("hook session-start: update claude_session_id failed", "error", err)
		}
		if err := h.store.UpdateSessionStatus(sess.ID, "active"); err != nil {
			h.logger.Warn("hook session-start: update status to active failed", "error", err)
		}
		h.logger.Info("hook session-start: session activated", "tgcc_session_id", sess.ID, "claude_session_id", sessionID, "correlation_id", correlationID)
	} else if cwd != "" {
		h.logger.Warn("hook session-start: no pending/spawning session found", "cwd", cwd, "correlation_id", correlationID)
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

	// Notify context monitor for context tracking/compaction and response relay.
	// RelayResponse handles dedup via message_offsets — direct send is removed
	// to prevent duplicate message delivery (H1 fix).
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
	// Validate and sanitize transcript path to prevent path traversal
	cleaned := filepath.Clean(transcriptPath)
	if !strings.HasPrefix(cleaned, filepath.Clean(homeClaudeProjects())) {
		h.logger.Warn("hook stop: transcript path outside allowed directory", "path", transcriptPath, "cleaned", cleaned)
		return ""
	}
	f, err := os.Open(cleaned)
	if err != nil {
		h.logger.Warn("hook stop: open transcript failed", "path", transcriptPath, "error", err)
		return ""
	}
	defer f.Close()

	// M3 fix: parse using the same nested structure as context/relay.go
	// Claude Code transcript format: {"type":"assistant","message":{"role":"assistant","content":[...]}}
	type contentBlock struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	}
	type msgPayload struct {
		Role    string         `json:"role"`
		Content []contentBlock `json:"content"`
	}
	type transcriptEntry struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
	}

	var lastAssistant string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry transcriptEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "assistant" {
			continue
		}
		var payload msgPayload
		if err := json.Unmarshal(entry.Message, &payload); err != nil {
			continue
		}
		var parts []string
		for _, block := range payload.Content {
			if block.Type == "text" && block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
		if len(parts) > 0 {
			lastAssistant = strings.Join(parts, "\n")
		}
	}
	if err := scanner.Err(); err != nil {
		h.logger.Warn("hook stop: scanner error reading transcript", "path", transcriptPath, "error", err)
	}
	if len(lastAssistant) > 4000 {
		lastAssistant = lastAssistant[:4000]
	}
	return lastAssistant
}

func homeClaudeProjects() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	}
	return filepath.Join(home, ".claude", "projects")
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
