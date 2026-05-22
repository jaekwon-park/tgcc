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

// MessageForwarder injects a message into a running session.
// Used by Stop hook to drain .notify-queue after transitioning to idle.
type MessageForwarder interface {
	ForwardMessage(ctx context.Context, sessionID string, text string) error
}

// Handlers processes different hook event types.
type Handlers struct {
	logger    *slog.Logger
	store     *store.Store
	sender    *bot.Sender
	monitor   ContextMonitor
	forwarder MessageForwarder
}

// NewHandlers creates new hook Handlers.
func NewHandlers(logger *slog.Logger, st *store.Store, sender *bot.Sender, monitor ContextMonitor, forwarder MessageForwarder) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{logger: logger, store: st, sender: sender, monitor: monitor, forwarder: forwarder}
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

	// Return 200 immediately so Claude Code is not blocked waiting for relay.
	// All post-response work (status transition, queue drain, context monitor)
	// runs in a goroutine to avoid blocking the HTTP handler.
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})

	// Transition session status active→idle and drain .notify-queue if needed.
	// Runs in a goroutine to avoid tail latency on the HTTP handler goroutine.
	go h.handleStopAsync(r.Context(), sessionID, transcriptPath)
}

// handleStopAsync performs the post-response Stop hook work: session lookup,
// active→idle transition, guaranteed queue drain, and context-monitor callback.
func (h *Handlers) handleStopAsync(ctx context.Context, sessionID, transcriptPath string) {
	if h.store == nil {
		return
	}

	sess, err := h.store.SessionByClaudeID(sessionID)
	if err != nil {
		h.logger.Warn("hook stop: lookup by claude_session_id failed", "error", err, "session_id", sessionID)
	}
	if sess == nil {
		sess, err = h.store.SessionByID(sessionID)
		if err != nil {
			h.logger.Warn("hook stop: lookup by id failed", "error", err, "session_id", sessionID)
		} else {
			h.logger.Debug("hook stop: resolved via fallback tgcc ID (not claude_session_id)", "session_id", sessionID)
		}
	}
	if sess == nil {
		return
	}

	// Transition active→idle (CAS) so the .notify-queue watcher can detect
	// the leader is waiting for new input.
	if err := h.store.UpdateSessionStatusIf(sess.ID, "active", "idle"); err != nil {
		h.logger.Warn("hook stop: update status active→idle failed", "error", err, "session_id", sess.ID)
	} else {
		h.logger.Debug("hook stop: session transitioned active→idle", "session_id", sess.ID)

		// Guaranteed queue drain: check .notify-queue now that the session
		// is idle. This handles items that arrived while the session was
		// busy — fsnotify skips those because the session wasn't idle, and
		// the event never re-fires. The fsnotify watcher remains as a
		// fast-path for items that arrive while already idle.
		if h.maybeDrainQueue(ctx, sess) {
			h.logger.Info("hook stop: queue drained on idle transition",
				"session_id", sess.ID,
				"workspace", sess.WorkspacePath,
			)
		}
	}

	// Context monitor (compaction tracking, etc.)
	if h.monitor != nil {
		if err := h.monitor.OnStopHook(context.Background(), sessionID, transcriptPath); err != nil {
			h.logger.Warn("context monitor OnStopHook failed", "error", err)
		}
	}
}

// maybeDrainQueue checks whether the session's workspace has a non-empty
// .notify-queue file and, if the forwarder is available, injects the
// [queue-drain] trigger message. Returns true if a drain was attempted.
func (h *Handlers) maybeDrainQueue(ctx context.Context, sess *store.Session) bool {
	if h.forwarder == nil || sess == nil || sess.WorkspacePath == "" {
		return false
	}
	queuePath := filepath.Join(sess.WorkspacePath, ".notify-queue")
	info, err := os.Stat(queuePath)
	if err != nil {
		// File doesn't exist or can't be accessed — nothing to drain.
		return false
	}
	if info.Size() == 0 {
		return false
	}
	h.logger.Debug("hook stop: .notify-queue non-empty, injecting drain trigger",
		"workspace", sess.WorkspacePath,
		"queue_bytes", info.Size(),
	)
	if err := h.forwarder.ForwardMessage(ctx, sess.ID, "[queue-drain]"); err != nil {
		h.logger.Warn("hook stop: queue drain ForwardMessage failed",
			"error", err, "session_id", sess.ID,
		)
		return false
	}
	return true
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
