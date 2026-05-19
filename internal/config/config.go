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
)

// ContextConfig holds context lifecycle thresholds.
type ContextConfig struct {
	SoftWarnBytes      int64 `toml:"soft_warn_bytes"`
	HardCompactBytes   int64 `toml:"hard_compact_bytes"`
	FreshRestartBytes  int64 `toml:"fresh_restart_bytes"`
	SoftWarnTurns      int64 `toml:"soft_warn_turns"`
	HardCompactTurns   int64 `toml:"hard_compact_turns"`
	IdleHibernateMin   int64 `toml:"idle_hibernate_min"`
}

// DefaultContextConfig returns sensible defaults for context lifecycle.
func DefaultContextConfig() ContextConfig {
	return ContextConfig{
		SoftWarnBytes:     80000,
		HardCompactBytes:  150000,
		FreshRestartBytes: 300000,
		SoftWarnTurns:     60,
		HardCompactTurns:  100,
		IdleHibernateMin:  30,
	}
}

// Config holds all tgcc configuration values.
type Config struct {
	// Required
	TelegramBotToken string

	// Optional with defaults
	DBPath      string
	HookPort    int
	HookToken   string
	LogLevel    string
	TmuxBin     string
	ClaudeBin   string
	TmuxSession string

	// Derived
	HomeDir string
	TgccDir string

	// Context lifecycle thresholds
	Context ContextConfig
}

// tomlFile is the on-disk representation of tgcc.toml.
type tomlFile struct {
	Context ContextConfig `toml:"context"`
}

// Load reads .env from ~/.tgcc/.env and returns parsed Config.
func Load() (*Config, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("could not determine home directory: %w", err)
	}
	tgccDir := filepath.Join(homeDir, ".tgcc")

	if err := ensureDir(tgccDir); err != nil {
		return nil, fmt.Errorf("could not create tgcc directory: %w", err)
	}

	envPath := filepath.Join(tgccDir, ".env")
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
		DBPath:           getEnvOrDefault(m, "TGCC_DB_PATH", filepath.Join(tgccDir, "state.db")),
		HookPort:         hookPort,
		HookToken:        getEnvOrDefault(m, "TGCC_HOOK_TOKEN", ""),
		LogLevel:         getEnvOrDefault(m, "TGCC_LOG_LEVEL", "info"),
		TmuxBin:          getEnvOrDefault(m, "TGCC_TMUX_BIN", "tmux"),
		ClaudeBin:        getEnvOrDefault(m, "TGCC_CLAUDE_BIN", "claude"),
		TmuxSession:      getEnvOrDefault(m, "TGCC_TMUX_SESSION", ""),
		HomeDir:          homeDir,
		TgccDir:          tgccDir,
		Context:          DefaultContextConfig(),
	}

	tomlPath := filepath.Join(tgccDir, "tgcc.toml")
	if err := loadTOML(tomlPath, cfg); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("could not load tgcc.toml: %w", err)
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
