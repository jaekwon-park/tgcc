// Package config handles environment and configuration file loading.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/jaekwon-park/tgcc/internal/honcho"
)

// ContextConfig holds context lifecycle thresholds.
type ContextConfig struct {
	SoftWarnBytes     int64 `toml:"soft_warn_bytes"`
	HardCompactBytes  int64 `toml:"hard_compact_bytes"`
	FreshRestartBytes int64 `toml:"fresh_restart_bytes"`
	SoftWarnTurns     int64 `toml:"soft_warn_turns"`
	HardCompactTurns  int64 `toml:"hard_compact_turns"`
	IdleHibernateMin  int64 `toml:"idle_hibernate_min"`
}

// DefaultContextConfig returns sensible defaults for context lifecycle.
// Byte thresholds are scaled to ~3 bytes/token observed in JSONL transcripts,
// targeting 75/85/95% of the 1M-context window used by Opus 4.7 / Claude Max.
// Override per-deployment in tgcc.toml [context] when running smaller models.
func DefaultContextConfig() ContextConfig {
	return ContextConfig{
		SoftWarnBytes:     2250000,
		HardCompactBytes:  2550000,
		FreshRestartBytes: 2850000,
		SoftWarnTurns:     60,
		HardCompactTurns:  100,
		IdleHibernateMin:  30,
	}
}

// TmuxConfig holds tmux-related configuration from tgcc.toml.
type TmuxConfig struct {
	SessionName string `toml:"session_name"`
	Bin         string `toml:"bin"`
}

// ClaudeConfig holds claude binary configuration from tgcc.toml.
type ClaudeConfig struct {
	Bin          string `toml:"bin"`
	SpawnTimeout int    `toml:"spawn_timeout"`
}

// WorkspaceConfig holds workspace scanning configuration from tgcc.toml.
type WorkspaceConfig struct {
	Roots []string `toml:"roots"`
}

// SpawnConfig holds settings applied to every Claude session tgcc spawns.
// Env is merged on top of the bot's inherited process environment when tmux
// new-window runs `claude`, so it can extend PATH (e.g. to reach ~/.local/bin
// for rtk, npm-global tools) or inject any other variable the workspace needs.
// Per-spawn keys like TGCC_CORRELATION_ID are added on top of this map and
// always win.
type SpawnConfig struct {
	Env map[string]string `toml:"env"`
}

// Config holds all tgcc configuration values.
type Config struct {
	// Required
	TelegramBotToken string

	// Optional with defaults
	DBPath       string
	HookPort     int
	HookToken    string
	LogLevel     string
	TmuxBin      string
	ClaudeBin    string
	TmuxSession  string
	TgccTomlPath string

	// Derived
	HomeDir string
	TgccDir string

	// Context lifecycle thresholds
	Context ContextConfig

	// Honcho integration
	Honcho honcho.HonchoConfig

	// TOML sections
	Tmux      TmuxConfig
	Claude    ClaudeConfig
	Workspace WorkspaceConfig
	Spawn     SpawnConfig
}

// tomlFile is the on-disk representation of tgcc.toml.
type tomlFile struct {
	Context   ContextConfig       `toml:"context"`
	Honcho    honcho.HonchoConfig `toml:"honcho"`
	Tmux      TmuxConfig          `toml:"tmux"`
	Claude    ClaudeConfig        `toml:"claude"`
	Workspace WorkspaceConfig     `toml:"workspace"`
	Spawn     SpawnConfig         `toml:"spawn"`
}

// Load reads .env from the binary directory and returns parsed Config.
func Load() (*Config, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not determine home directory: %w", err)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("could not determine executable path: %w", err)
	}
	exeDir := filepath.Dir(exe)

	// Keep tgccDir for any legacy usage; primary config lives in exeDir.
	tgccDir := exeDir

	envPath := filepath.Join(exeDir, ".env")
	m, err := parseEnvFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("could not parse env file: %w", err)
	}
	if m == nil {
		m = make(map[string]string)
	}

	token := getEnvOrDefault(m, "TELEGRAM_BOT_TOKEN", "")
	if token == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required but not set in %s", envPath)
	}

	hookPortStr := getEnvOrDefault(m, "TGCC_HOOK_PORT", "47829")
	hookPort, err := strconv.Atoi(hookPortStr)
	if err != nil {
		return nil, fmt.Errorf("invalid TGCC_HOOK_PORT value %q: %w", hookPortStr, err)
	}

	cfg := &Config{
		TelegramBotToken: token,
		DBPath:           getEnvOrDefault(m, "TGCC_DB_PATH", filepath.Join(exeDir, "state.db")),
		HookPort:         hookPort,
		HookToken:        getEnvOrDefault(m, "TGCC_HOOK_TOKEN", ""),
		LogLevel:         getEnvOrDefault(m, "TGCC_LOG_LEVEL", "info"),
		TmuxBin:          getEnvOrDefault(m, "TGCC_TMUX_BIN", "tmux"),
		ClaudeBin:        getEnvOrDefault(m, "TGCC_CLAUDE_BIN", "claude"),
		TmuxSession:      getEnvOrDefault(m, "TGCC_TMUX_SESSION", ""),
		TgccTomlPath:     getEnvOrDefault(m, "TGCC_TOML_PATH", filepath.Join(exeDir, "tgcc.toml")),
		HomeDir:          homeDir,
		TgccDir:          tgccDir,
		Context:          DefaultContextConfig(),
		Honcho:           honcho.DefaultHonchoConfig(),
	}

	if err := loadTOML(cfg.TgccTomlPath, cfg); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("could not load tgcc.toml: %w", err)
	}

	// Apply TOML fallbacks for fields that can come from env or TOML
	// Priority: env > toml > hardcoded default
	if cfg.Tmux.SessionName != "" && cfg.TmuxSession == "" {
		cfg.TmuxSession = cfg.Tmux.SessionName
	}
	if cfg.Tmux.Bin != "" && cfg.TmuxBin == "tmux" {
		cfg.TmuxBin = cfg.Tmux.Bin
	}
	if cfg.Claude.Bin != "" && cfg.ClaudeBin == "claude" {
		cfg.ClaudeBin = cfg.Claude.Bin
	}
	if cfg.Claude.SpawnTimeout == 0 {
		cfg.Claude.SpawnTimeout = 30
	}

	return cfg, nil
}

// loadTOML reads tgcc.toml and merges the [context] section into cfg.
// Returns os.ErrNotExist if the file does not exist.
func loadTOML(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var tf tomlFile
	if _, err := toml.NewDecoder(f).Decode(&tf); err != nil {
		return fmt.Errorf("parse toml: %w", err)
	}

	// Merge only non-zero values so unset fields keep their defaults.
	if tf.Context.SoftWarnBytes > 0 {
		cfg.Context.SoftWarnBytes = tf.Context.SoftWarnBytes
	}
	if tf.Context.HardCompactBytes > 0 {
		cfg.Context.HardCompactBytes = tf.Context.HardCompactBytes
	}
	if tf.Context.FreshRestartBytes > 0 {
		cfg.Context.FreshRestartBytes = tf.Context.FreshRestartBytes
	}
	if tf.Context.SoftWarnTurns > 0 {
		cfg.Context.SoftWarnTurns = tf.Context.SoftWarnTurns
	}
	if tf.Context.HardCompactTurns > 0 {
		cfg.Context.HardCompactTurns = tf.Context.HardCompactTurns
	}
	if tf.Context.IdleHibernateMin > 0 {
		cfg.Context.IdleHibernateMin = tf.Context.IdleHibernateMin
	}

	// Merge non-default Honcho fields.
	if tf.Honcho.Enabled {
		cfg.Honcho.Enabled = tf.Honcho.Enabled
	}
	if tf.Honcho.BaseURL != "" {
		cfg.Honcho.BaseURL = tf.Honcho.BaseURL
	}
	if tf.Honcho.Workspace != "" {
		cfg.Honcho.Workspace = tf.Honcho.Workspace
	}
	// Merge Tmux
	if tf.Tmux.SessionName != "" {
		cfg.Tmux.SessionName = tf.Tmux.SessionName
	}
	if tf.Tmux.Bin != "" {
		cfg.Tmux.Bin = tf.Tmux.Bin
	}
	// Merge Claude
	if tf.Claude.Bin != "" {
		cfg.Claude.Bin = tf.Claude.Bin
	}
	if tf.Claude.SpawnTimeout > 0 {
		cfg.Claude.SpawnTimeout = tf.Claude.SpawnTimeout
	}
	// Merge Workspace
	if len(tf.Workspace.Roots) > 0 {
		cfg.Workspace.Roots = tf.Workspace.Roots
	}
	// Merge Spawn env. Last writer wins so the toml file overrides anything
	// previously seeded into cfg.Spawn.Env (today nothing seeds it, but keeps
	// the merge consistent with the other sections above).
	if len(tf.Spawn.Env) > 0 {
		if cfg.Spawn.Env == nil {
			cfg.Spawn.Env = make(map[string]string, len(tf.Spawn.Env))
		}
		for k, v := range tf.Spawn.Env {
			cfg.Spawn.Env[k] = v
		}
	}
	return nil
}

// ensureDir creates a directory if it doesn't exist.
func ensureDir(path string) error {
	return os.MkdirAll(path, 0700)
}

// parseEnvFile reads a simple KEY=VALUE .env file.
func parseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		if key != "" {
			m[key] = val
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading env file: %w", err)
	}
	return m, nil
}

// getEnvOrDefault returns env value or fallback.
func getEnvOrDefault(m map[string]string, key, fallback string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return fallback
}

// TopicConfig represents a single [[topic]] entry in tgcc.toml.
type TopicConfig struct {
	ThreadID        int64  `toml:"thread_id"`
	HonchoSessionID string `toml:"honcho_session_id"`
	Model           string `toml:"model"`
	WorkspacePath   string `toml:"workspace_path"`
	// RequireMention gates this topic: when true the bot only responds to
	// messages that @mention it or reply to one of its messages.
	RequireMention bool `toml:"require_mention"`
}

// GroupConfig represents a single [[group]] entry in tgcc.toml.
type GroupConfig struct {
	ChatID int64         `toml:"chat_id"`
	Name   string        `toml:"name"` // 워크스페이스 경로용 (예: "ccgram")
	Topics []TopicConfig `toml:"topic"`
}

// TgccToml represents the root of tgcc.toml.
type TgccToml struct {
	Groups []GroupConfig `toml:"group"`
}

// LoadTgccToml reads and parses the tgcc.toml configuration file.
// Returns nil if the file does not exist (no error).
func LoadTgccToml(path string) (*TgccToml, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read tgcc.toml: %w", err)
	}
	var cfg TgccToml
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse tgcc.toml: %w", err)
	}
	return &cfg, nil
}
