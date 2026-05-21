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

// ActivationCallback is called after a session is successfully activated
// (claude_session_id set, status set to active). The Manager uses this to
// inject prior Honcho context into the newly active session.
type ActivationCallback interface {
	OnSessionActivated(ctx context.Context, sessionID string)
}

// Handlers processes different hook event types.
type Handlers struct {
	logger     *slog.Logger
	store      *store.Store
	sender     *bot.Sender
	monitor    ContextMonitor
	onActivate ActivationCallback
}

// NewHandlers creates new hook Handlers.
func NewHandlers(logger *slog.Logger, st *store.Store, sender *bot.Sender, monitor ContextMonitor) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{logger: logger, store: st, sender: sender, monitor: monitor}
}

// SetActivationCallback registers a callback to be invoked after a session is
// activated via the session-start hook.
func (h *Handlers) SetActivationCallback(cb ActivationCallback) {
	h.onActivate = cb
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

	// Fall back to cwd+null-claude-id matching if correlation_id didn't find a match.
	// Fix: use claude_session_id IS NULL instead of specific statuses.
	// The old SessionByWorkspaceAndStatus(cwd, []string{"pending", "spawning"}) had
	// a race condition: the 2-second timer could transition spawning→active before
	// the hook fires, causing the lookup to miss and leaving claude_session_id as NULL.
	if sess == nil && cwd != "" && h.store != nil {
		var lookupErr error
		sess, lookupErr = h.store.SessionByWorkspaceNullClaudeID(cwd)
		if lookupErr != nil {
			h.logger.Warn("hook session-start: lookup session by workspace+null claude_id failed", "error", lookupErr, "cwd", cwd)
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

		// Inject prior Honcho context into the newly active session asynchronously.
		if h.onActivate != nil {
			go h.onActivate.OnSessionActivated(context.Background(), sess.ID)
		}
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

	// Return 200 immediately so Claude Code is not blocked waiting for relay.
	// OnStopHook (context-size tracking + compaction) runs in a goroutine.
	// Response relaying is handled separately by the transcript Poller.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	if h.monitor != nil {
		monitor := h.monitor
		logger := h.logger
		go func() {
			if err := monitor.OnStopHook(context.Background(), sessionID, transcriptPath); err != nil {
				logger.Warn("context monitor OnStopHook failed", "error", err)
			}
		}()
	}
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

	// Drop idle notifications that fire after every Claude response.
	// "--dangerously-skip-permissions" means permission prompts never appear,
	// so "waiting for your input" is the only notification that fires in practice.
	if strings.Contains(strings.ToLower(message), "waiting for your input") {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}

	if sessionID != "" && message != "" && h.store != nil && h.sender != nil {
		// Same Claude-UUID-vs-tgcc-PK issue as the Stop hook: the hook payload
		// session_id is Claude Code's internal UUID, populated as
		// claude_session_id by the SessionStart hook. Look that up first, fall
		// back to SessionByID for callers that already know the tgcc id.
		sess, err := h.store.SessionByClaudeID(sessionID)
		if err != nil {
			h.logger.Warn("hook notification: lookup by claude_session_id failed", "error", err, "session_id", sessionID)
		}
		if sess == nil {
			sess, err = h.store.SessionByID(sessionID)
			if err != nil {
				h.logger.Warn("hook notification: lookup by id failed", "error", err, "session_id", sessionID)
			}
		}
		if sess != nil {
			topic, terr := h.store.TopicByID(sess.TopicID)
			if terr != nil {
				h.logger.Warn("hook notification: topic lookup failed", "error", terr, "topic_id", sess.TopicID)
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
