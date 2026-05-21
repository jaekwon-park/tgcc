// Package hook provides the HTTP server for Claude Code hook events and internal API.
package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/store"
)

// SessionProvider is the interface for querying session state.
type SessionProvider interface {
	ActiveSessionCount() int
}

// Server listens for Claude Code hook events and serves internal API on localhost.
type Server struct {
	mu       sync.Mutex
	mux      *http.ServeMux
	httpSrv  *http.Server
	port     int
	token    string
	logger   *slog.Logger
	sessions SessionProvider

	startTime time.Time

	// hook handlers
	handlers *Handlers

	running atomic.Bool
}

// NewServer creates a new hook Server.
func NewServer(port int, token string, logger *slog.Logger, st *store.Store, sender *bot.Sender, monitor ContextMonitor) *Server {
	s := &Server{
		port:      port,
		token:     token,
		logger:    logger,
		startTime: time.Now(),
		handlers:  NewHandlers(logger, st, sender, monitor),
	}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/hooks/session-start", s.handleSessionStart)
	s.mux.HandleFunc("/hooks/stop", s.handleStop)
	s.mux.HandleFunc("/hooks/notification", s.handleNotification)
	s.mux.HandleFunc("/sessions", s.handleSessions)
	return s
}

// SetSessionProvider sets the session provider for status queries.
func (s *Server) SetSessionProvider(p SessionProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = p
}

// SetActivationCallback delegates to the underlying Handlers.
func (s *Server) SetActivationCallback(cb ActivationCallback) {
	s.handlers.SetActivationCallback(cb)
}

// Start begins listening on the configured port.
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	s.httpSrv = &http.Server{
		Addr:         addr,
		Handler:      s.authMiddleware(s.mux),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	}
	s.running.Store(true)
	s.logger.Info("hook server starting", "addr", addr)

	go func() {
		<-ctx.Done()
		s.running.Store(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := s.httpSrv.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("hook server shutdown error", "error", err)
		}
	}()

	if err := s.httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.running.Store(false)
		return fmt.Errorf("hook server listen: %w", err)
	}
	return nil
}

// authMiddleware validates X-tgcc-Token for protected endpoints.
// /healthz is exempt from auth when token is set.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health check is always accessible
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		if s.token != "" {
			provided := r.Header.Get("X-tgcc-Token")
			if provided != s.token {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// handleHealthz returns the server health status.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	sessionCount := 0
	s.mu.Lock()
	if s.sessions != nil {
		sessionCount = s.sessions.ActiveSessionCount()
	}
	s.mu.Unlock()

	resp := map[string]interface{}{
		"status":         "ok",
		"uptime_seconds": int(time.Since(s.startTime).Seconds()),
		"session_count":  sessionCount,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSessionStart handles POST /hooks/session-start
func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	s.handlers.HandleSessionStart(w, r)
}

// handleStop handles POST /hooks/stop
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.handlers.HandleStop(w, r)
}

// handleNotification handles POST /hooks/notification
func (s *Server) handleNotification(w http.ResponseWriter, r *http.Request) {
	s.handlers.HandleNotification(w, r)
}

// handleSessions handles GET /sessions (list all sessions)
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessions": []interface{}{},
		"total":    0,
	})
}
