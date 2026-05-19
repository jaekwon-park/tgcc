// tgcc - Telegram-controlled Claude Code orchestrator using tmux.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jaekwon-park/tgcc/internal/config"
	"github.com/jaekwon-park/tgcc/internal/session"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

func main() {
	// Set up structured logging
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	slog.Info("tgcc starting",
		"hook_port", cfg.HookPort,
		"db_path", cfg.DBPath,
		"tmux_session", cfg.TmuxSessionName,
	)

	// Open SQLite store
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		slog.Error("failed to open store", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	// Create tmux adapter
	tmuxAdapter := tmux.NewAdapter(cfg.TmuxBinary)

	// Set up context with graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// --- Phase 1: Boot-time reconciliation ---
	slog.Info("running boot-time reconciliation...")
	reconciler := session.NewReconciler(st, tmuxAdapter, nil) // nil sender for now
	results, err := reconciler.Reconcile(ctx)
	if err != nil {
		slog.Error("reconciliation failed", "error", err)
	} else {
		for _, r := range results {
			slog.Info("reconciled",
				"session_id", r.SessionID,
				"action", r.Action,
				"topic_id", r.TopicID,
			)
		}
	}

	// --- Phase 2: Start supervisor ---
	supervisor := session.NewSupervisor(session.SupervisorConfig{
		Store:      st,
		Tmux:       tmuxAdapter,
		Sender:     nil, // TODO: wire up real sender
		MaxRetries: cfg.CrashRestartMaxRetries,
	})

	go func() {
		if err := supervisor.Run(ctx); err != nil && err != context.Canceled {
			slog.Error("supervisor error", "error", err)
		}
	}()

	slog.Info("tgcc running, waiting for signals...")

	// Wait for shutdown signal
	<-sigCh
	slog.Info("received shutdown signal, stopping...")
	cancel()

	fmt.Println("tgcc stopped.")
}
