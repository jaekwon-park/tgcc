// Package config loads and validates tgcc configuration from .env and tgcc.toml.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config holds all runtime configuration.
type Config struct {
	TelegramBotToken string
	HookToken        string
	LogLevel         string
	DBPath           string
	HookPort         int

	// Session settings
	SpawnTimeoutSeconds    int
	IdleThresholdSeconds   int
	CrashRestartMaxRetries int

	// Tmux settings
	TmuxSessionName string
	TmuxBinary      string

	// Claude settings
	ClaudeBinary string
	ClaudeResume bool

	// Workspace roots
	WorkspaceRoots []string
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		LogLevel:               "info",
		DBPath:                 filepath.Join(home, ".tgcc", "state.db"),
		HookPort:               47829,
		SpawnTimeoutSeconds:    5,
		IdleThresholdSeconds:   300,
		CrashRestartMaxRetries: 3,
		TmuxSessionName:        "tgcc",
		TmuxBinary:             "tmux",
		ClaudeBinary:           "claude",
		WorkspaceRoots:         []string{},
	}
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Load from environment
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		cfg.TelegramBotToken = v
	}
	if v := os.Getenv("TGCC_HOOK_TOKEN"); v != "" {
		cfg.HookToken = v
	}
	if v := os.Getenv("TGCC_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("TGCC_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("TGCC_HOOK_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("invalid TGCC_HOOK_PORT: %w", err)
		}
		cfg.HookPort = p
	}

	// Validate required fields
	if cfg.TelegramBotToken == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.HookToken == "" {
		return nil, fmt.Errorf("TGCC_HOOK_TOKEN is required")
	}

	// Ensure DB directory exists
	dbDir := filepath.Dir(cfg.DBPath)
	if err := os.MkdirAll(dbDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create DB directory %s: %w", dbDir, err)
	}

	return cfg, nil
}

// MaskToken masks a token for logging (show last 4 chars only).
func MaskToken(t string) string {
	if len(t) <= 4 {
		return "****"
	}
	return strings.Repeat("*", len(t)-4) + t[len(t)-4:]
}
