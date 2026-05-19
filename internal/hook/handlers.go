package hook

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
)

// ContextMonitor is the interface for context lifecycle monitoring.
type ContextMonitor interface {
	OnStopHook(ctx context.Context, sessionID, transcriptPath string, chatID, threadID int64) error
}

// Handlers processes different hook event types.
type Handlers struct {
	logger  *slog.Logger
	monitor ContextMonitor
}

// NewHandlers creates new hook Handlers.
func NewHandlers(logger *slog.Logger, monitor ContextMonitor) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{logger: logger, monitor: monitor}
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
	h.logger.Info("hook session-start received",
		"session_id", payload["session_id"],
		"cwd", payload["cwd"],
	)
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
	h.logger.Info("hook stop received", "session_id", payload["session_id"])

	if h.monitor != nil {
		sessionID, _ := payload["session_id"].(string)
		transcriptPath, _ := payload["transcript_path"].(string)
		chatIDFloat, _ := payload["chat_id"].(float64)
		threadIDFloat, _ := payload["thread_id"].(float64)
		if err := h.monitor.OnStopHook(context.Background(), sessionID, transcriptPath, int64(chatIDFloat), int64(threadIDFloat)); err != nil {
			h.logger.Error("context monitor OnStopHook failed", "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
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
	h.logger.Info("hook notification received",
		"session_id", payload["session_id"],
		"message", payload["message"],
	)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
