// tgcc — Telegram Forum Topics ↔ Claude Code bridge.
// Single binary: tgcc init | tgcc pair <code> | tgcc serve
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jaekwon-park/tgcc/internal/acl"
	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	tmuxctx "github.com/jaekwon-park/tgcc/internal/context"
	"github.com/jaekwon-park/tgcc/internal/honcho"
	"github.com/jaekwon-park/tgcc/internal/hook"
	"github.com/jaekwon-park/tgcc/internal/router"
	"github.com/jaekwon-park/tgcc/internal/session"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

const version = "0.2.0"

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

// parseLogLevel converts a string log level to slog.Level.
func parseLogLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `tgcc — Telegram Forum Topics ↔ Claude Code 브릿지

사용법:
  tgcc init              초기 설정 (바이너리 디렉토리에 .env 생성)
  tgcc pair <코드>       페어링 코드로 인증 완료
  tgcc serve             데몬 실행 (봇 + Hook 서버 시작)
  tgcc status            실행 중인 tgcc 상태 확인
  tgcc version           버전 출력

예시:
  tgcc init
  tgcc pair 738291
  tgcc serve

`)
}

// cmdInit initializes the binary directory with .env template and migrations.
func cmdInit() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 실행 파일 경로 확인 실패: %v\n", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exe)

	// Ensure migrations/ directory exists next to the binary
	migrationsDir := filepath.Join(exeDir, "migrations")
	if err := os.MkdirAll(migrationsDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "❌ migrations 디렉터리 생성 실패: %v\n", err)
		os.Exit(1)
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		fmt.Fprintf(os.Stderr, "❌ 랜덤 토큰 생성 실패: %v\n", err)
		os.Exit(1)
	}
	hookToken := hex.EncodeToString(tokenBytes)

	envPath := filepath.Join(exeDir, ".env")
	envText := fmt.Sprintf("TELEGRAM_BOT_TOKEN=YOUR_BOT_TOKEN_HERE\nTGCC_HOOK_TOKEN=%s\nTGCC_LOG_LEVEL=info\nTGCC_DB_PATH=%s\nTGCC_HOOK_PORT=47829\n", hookToken, filepath.Join(exeDir, "state.db"))
	if err := os.WriteFile(envPath, []byte(envText), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "❌ .env 파일 생성 실패: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("✅ 초기화 완료: %s\n", exeDir)
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
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 설정 로드 실패: %v\n", err)
		os.Exit(1)
	}
	logLevel := parseLogLevel(cfg.LogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	logger.Info("tgcc starting", "version", version, "log_level", cfg.LogLevel)
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

// cmdStatus queries the daemon health via the internal HTTP API.
func cmdStatus() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 설정 로드 실패: %v\n", err)
		os.Exit(1)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", cfg.HookPort)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 요청 생성 실패\n")
		os.Exit(1)
	}
	if cfg.HookToken != "" {
		req.Header.Set("X-tgcc-Token", cfg.HookToken)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ tgcc가 실행 중이 아닙니다. tgcc serve 로 시작하세요.\n")
		os.Exit(1)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "❌ tgcc 응답 오류 (상태 코드: %d)\n", resp.StatusCode)
		os.Exit(1)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 응답 읽기 실패\n")
		os.Exit(1)
	}

	// Pretty-print JSON response
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Println(string(body))
		return
	}

	status, _ := result["status"].(string)
	uptime, _ := result["uptime_seconds"].(float64)
	sessionCount, _ := result["session_count"].(float64)
	fmt.Printf("📊 tgcc 상태\n\n")
	fmt.Printf("상태: %s\n", status)
	if uptime > 0 {
		fmt.Printf("가동 시간: %.0f초\n", uptime)
	}
	if sessionCount > 0 {
		fmt.Printf("활성 세션: %.0f개\n", sessionCount)
	}
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

	// Sync topics from tgcc.toml
	tomlCfg, tomlErr := config.LoadTgccToml(cfg.TgccTomlPath)
	if tomlErr != nil {
		logger.Warn("failed to load tgcc.toml, skipping topic sync", "error", tomlErr)
	} else if tomlCfg != nil && len(tomlCfg.Groups) > 0 {
		synced := 0
		for _, g := range tomlCfg.Groups {
			for _, tc := range g.Topics {
				topicID, err := st.UpsertTopicFull(g.ChatID, tc.ThreadID, tc.HonchoSessionID, tc.Model, tc.WorkspacePath)
				if err != nil {
					logger.Warn("topic upsert failed", "chat_id", g.ChatID, "thread_id", tc.ThreadID, "error", err)
					continue
				}
				synced++
				logger.Debug("topic synced from tgcc.toml", "topic_id", topicID, "chat_id", g.ChatID, "thread_id", tc.ThreadID)
			}
		}
		logger.Info("tgcc.toml topic sync complete", "synced", synced)
	}

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

	// 5. Bot client & sender (moved before reconciler so crash notifications fire at startup)
	client := bot.NewClient(cfg.TelegramBotToken)
	sender := bot.NewSender(client, logger)
	go func() {
		if err := sender.Start(ctx); err != nil {
			logger.Error("sender stopped", "error", err)
		}
	}()

	// 4. Reconciler + Supervisor (M3)
	reconciler := session.NewReconciler(st, tmuxAdapter, sender, logger)
	if err := reconciler.Run(ctx, tmuxSessionName); err != nil {
		logger.Warn("reconciler run failed", "error", err)
	}
	// 6. ACL & pairing
	guard := acl.NewGuard(st, logger)
	pairingMgr := acl.NewPairingManager(st)

	// 7. Session manager
	workspaceRoot := cfg.HomeDir
	sessionMgr := session.NewManager(st, tmuxAdapter, logger, sender, tmuxSessionName, claudeBin, workspaceRoot, cfg.Workspace.Roots)

	// 4c. Context lifecycle monitor (M6)
	ctxMon := tmuxctx.NewMonitor(st, tmuxAdapter, sender, cfg.Context, logger)

	// 4b. Hook server — internal HTTP API + Claude Code hook receiver
	hookSrv := hook.NewServer(cfg.HookPort, cfg.HookToken, logger, st, sender, ctxMon)
	go func() {
		if err := hookSrv.Start(ctx); err != nil {
			logger.Error("hook server failed", "error", err)
		}
	}()

	// Wire session provider to hook server for status queries
	hookSrv.SetSessionProvider(sessionMgr)

	// 7b. Honcho client
	honchoClient := honcho.New(cfg.Honcho)

	// 7c. Supervisor (M3) — restart crashed sessions periodically
	supervisor := session.NewSupervisor(st, sessionMgr, 0, cfg.Context, sender, honchoClient, logger)
	go supervisor.Start(ctx)

	// 8. Router
	r := router.NewRouter(st, logger, sender, guard, pairingMgr, sessionMgr, ctxMon, honchoClient)

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
