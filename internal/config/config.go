// Package config handles environment and configuration file loading.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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

	hookPortStr := getEnvOrDefault(m, "HOOK_PORT", "47829")
	hookPort, err := strconv.Atoi(hookPortStr)
	if err != nil {
		return nil, fmt.Errorf("invalid HOOK_PORT value %q: %w", hookPortStr, err)
	}

	cfg := &Config{
		TelegramBotToken: token,
		DBPath:           getEnvOrDefault(m, "DB_PATH", filepath.Join(tgccDir, "state.db")),
		HookPort:         hookPort,
		HookToken:        getEnvOrDefault(m, "HOOK_TOKEN", ""),
		LogLevel:         getEnvOrDefault(m, "LOG_LEVEL", "info"),
		TmuxBin:          getEnvOrDefault(m, "TMUX_BIN", "tmux"),
		ClaudeBin:        getEnvOrDefault(m, "CLAUDE_BIN", "claude"),
		TmuxSession:      getEnvOrDefault(m, "TMUX_SESSION", ""),
		HomeDir:          homeDir,
		TgccDir:          tgccDir,
	}
	return cfg, nil
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
