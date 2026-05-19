// tgcc — Telegram Forum Topics ↔ Claude Code bridge.
// Single binary: tgcc init | tgcc pair <code> | tgcc serve
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jaekwon-park/tgcc/internal/acl"
	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	"github.com/jaekwon-park/tgcc/internal/hook"
	"github.com/jaekwon-park/tgcc/internal/router"
	"github.com/jaekwon-park/tgcc/internal/session"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "init":
		cmdInit()
	case "pair":
		cmdPair()
	case "serve":
		cmdServe()
	case "status":
		cmdStatus()
	case "--version", "-version", "version":
		fmt.Printf("tgcc v%s\n", version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `tgcc — Telegram Forum Topics ↔ Claude Code bridge

Usage:
  tgcc init              Initialize tgcc configuration
  tgcc pair <code>       Complete pairing with a 6-digit code
  tgcc serve             Start the tgcc daemon
  tgcc status            Show daemon status
  tgcc version           Print version

`)
}

// cmdInit initializes ~/.tgcc/ with .env template and SQLite DB.
func cmdInit() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 홈 디렉터리 확인 실패: %v\n", err)
		os.Exit(1)
	}
	tgccDir := filepath.Join(homeDir, ".tgcc")
	if err := os.MkdirAll(tgccDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "❌ 디렉터리 생성 실패: %v\n", err)
		os.Exit(1)
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		fmt.Fprintf(os.Stderr, "❌ 랜덤 토큰 생성 실패: %v\n", err)
		os.Exit(1)
	}
	hookToken := hex.EncodeToString(tokenBytes)

	envPath := filepath.Join(tgccDir, ".env")
	envText := fmt.Sprintf("TELEGRAM_BOT_TOKEN=YOUR_BOT_TOKEN_HERE\nTGCC_HOOK_TOKEN=%s\nTGCC_LOG_LEVEL=info\nTGCC_DB_PATH=%s\nTGCC_HOOK_PORT=47829\n", hookToken, filepath.Join(tgccDir, "state.db"))
	if err := os.WriteFile(envPath, []byte(envText), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "❌ .env 파일 생성 실패: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ 초기화 완료: %s\n", tgccDir)
	fmt.Printf("✅ 환경 파일 생성: %s\n", envPath)
}

// cmdPair completes pairing: tgcc pair <code>
func cmdPair() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: tgcc pair <code>")
		os.Exit(1)
	}
	code := os.Args[2]
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 설정 로드 실패: %v\n", err)
		os.Exit(1)
	}
	st, err := store.New(cfg.DBPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 스토어 오픈 실패: %v\n", err)
		os.Exit(1)
	}
	defer st.Close()

	pairingMgr := acl.NewPairingManager(st)
	userID, err := pairingMgr.VerifyCode(context.Background(), code)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 페어링 실패: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 페어링 완료! User ID: %d\n", userID)
}

// cmdServe starts the main daemon.
func cmdServe() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if cfg.TelegramBotToken == "" {
		logger.Error("missing TELEGRAM_BOT_TOKEN")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := runServe(ctx, cfg, logger); err != nil {
		logger.Error("serve failed", "error", err)
		os.Exit(1)
	}
}

// cmdStatus queries the daemon health.
func cmdStatus() {
	// TODO: implement status command (query internal HTTP API)
	fmt.Println("tgcc status — not implemented yet")
}

// runServe is the core serve logic, separated for testability.
func runServe(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	// 1. Connect to SQLite
	st, err := store.New(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()
	logger.Info("sqlite connected", "path", cfg.DBPath)

	// 2. Set defaults
	tmuxBin := cfg.TmuxBin
	if tmuxBin == "" {
		tmuxBin = "tmux"
	}
	tmuxSessionName := cfg.TmuxSession
	if tmuxSessionName == "" {
		tmuxSessionName = "tgcc"
	}
	claudeBin := cfg.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}

	// 3. Tmux version check — exit if tmux < 3.0
	tmuxAdapter := tmux.NewAdapter(tmuxBin, tmuxSessionName, logger)
	if err := tmuxAdapter.CheckVersion(3, 0); err != nil {
		logger.Error("tmux version check failed", "error", err)
		return fmt.Errorf("tmux version check failed (3.0+ required): %w", err)
	}
	logger.Info("tmux version check passed")

	// Ensure tmux session exists (create if not)
	if err := ensureTmuxSession(tmuxBin, tmuxSessionName); err != nil {
		logger.Warn("could not ensure tmux session exists", "error", err)
	}

	// 4. Reconciler + Supervisor (M3)
	reconciler := session.NewReconciler(st, tmuxAdapter)
	if err := reconciler.Run(ctx, tmuxSessionName); err != nil {
		logger.Warn("reconciler run failed", "error", err)
	}
	_ = tmux.NewParser()
	_ = hook.NewServer()
	_ = hook.NewHandlers()

	// 5. Bot client & sender
	client := bot.NewClient(cfg.TelegramBotToken)
	sender := bot.NewSender(client, logger)
	go func() {
		if err := sender.Start(ctx); err != nil {
			logger.Error("sender stopped", "error", err)
		}
	}()

	// 6. ACL & pairing
	guard := acl.NewGuard(st, logger)
	pairingMgr := acl.NewPairingManager(st)

	// 7. Session manager
	workspaceRoot := cfg.HomeDir
	sessionMgr := session.NewManager(st, tmuxAdapter, logger, sender, tmuxSessionName, claudeBin, workspaceRoot)

	// 7b. Supervisor (M3) — restart crashed sessions periodically
	supervisor := session.NewSupervisor(st, sessionMgr, 0)
	go supervisor.Start(ctx)

	// 8. Router
	r := router.NewRouter(st, logger, sender, guard, pairingMgr, sessionMgr)

	// 9. Bot listener (long-polling)
	listener := bot.NewListener(client, logger)
	go func() {
		logger.Info("bot listener starting")
		if err := listener.Start(ctx); err != nil {
			logger.Error("listener stopped", "error", err)
		}
	}()

	// 10. Dispatch loop
	for {
		select {
		case <-ctx.Done():
			logger.Info("context cancelled, shutting down")
			listener.Stop()
			sender.Stop()
			return nil
		case update := <-listener.Updates():
			decision, user := guard.Check(ctx, update)
			switch decision {
			case acl.Allow:
				if err := r.Route(ctx, update, user); err != nil {
					logger.Error("route failed", "error", err, "update_id", update.UpdateID)
				}
			case acl.HandlePairing:
				if err := r.Route(ctx, update, nil); err != nil {
					logger.Error("route pairing failed", "error", err, "update_id", update.UpdateID)
				}
			default:
				var actorUserID *int64
				if update.Message != nil && update.Message.From != nil {
					id := update.Message.From.ID
					actorUserID = &id
				}
				reason := "unknown"
				switch decision {
				case acl.DenyUnknownUser:
					reason = "unknown_user"
				case acl.DenyRole:
					reason = "role_denied"
				case acl.DenyChatNotRegistered:
					reason = "chat_not_registered"
				}
				guard.RecordDenial(ctx, actorUserID, reason)
			}
		}
	}
}

// ensureTmuxSession creates the tmux session if it doesn't already exist.
func ensureTmuxSession(tmuxBin, sessionName string) error {
	// Check if session already exists
	check := exec.Command(tmuxBin, "has-session", "-t", sessionName)
	if check.Run() == nil {
		return nil
	}

	// Create detached session with a placeholder window
	create := exec.Command(tmuxBin, "new-session", "-d", "-s", sessionName, "-n", "idle")
	return create.Run()
}
