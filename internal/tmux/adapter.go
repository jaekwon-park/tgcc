// Package tmux provides tmux CLI abstraction.
package tmux

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const execTimeout = 5 * time.Second

// Adapter wraps tmux CLI commands.
type Adapter struct {
	tmuxBin     string
	sessionName string
	logger      *slog.Logger
}

// NewAdapter creates a new Tmux Adapter.
// tmuxBin defaults to "tmux" when empty; logger defaults to slog.Default() when nil.
func NewAdapter(tmuxBin, sessionName string, logger *slog.Logger) *Adapter {
	if tmuxBin == "" {
		tmuxBin = "tmux"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Adapter{
		tmuxBin:     tmuxBin,
		sessionName: sessionName,
		logger:      logger,
	}
}

// run executes a tmux subcommand with a 5-second timeout.
func (a *Adapter) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), execTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.tmuxBin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	a.logger.Debug("tmux exec", "args", args)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w (stderr: %s)", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// CheckVersion runs tmux -V and returns an error if the version is below minMajor.minMinor.
func (a *Adapter) CheckVersion(minMajor, minMinor int) error {
	out, err := a.run("-V")
	if err != nil {
		return fmt.Errorf("check version: %w", err)
	}
	// output: "tmux 3.4" or "tmux 3.4a"
	parts := strings.Fields(out)
	if len(parts) < 2 {
		return fmt.Errorf("unexpected tmux -V output: %q", out)
	}
	verParts := strings.SplitN(parts[1], ".", 2)
	major, err := strconv.Atoi(verParts[0])
	if err != nil {
		return fmt.Errorf("parse tmux major version %q: %w", parts[1], err)
	}
	minor := 0
	if len(verParts) == 2 {
		minorStr := strings.TrimRight(verParts[1], "abcdefghijklmnopqrstuvwxyz")
		if minorStr != "" {
			minor, _ = strconv.Atoi(minorStr)
		}
	}
	if major < minMajor || (major == minMajor && minor < minMinor) {
		return fmt.Errorf("tmux %d.%d required, got %s", minMajor, minMinor, parts[1])
	}
	return nil
}

// shellSingleQuote wraps s in single quotes, escaping any existing single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// NewWindow creates a new window in the given session and returns its WindowInfo.
func (a *Adapter) NewWindow(session, name, cmd string) (*WindowInfo, error) {
	return a.NewWindowWithEnv(session, name, "", cmd, nil, nil)
}

// NewWindowWithEnv creates a new tmux window with explicit working directory,
// environment variables, and properly shell-escaped command arguments.
//
// PATH is wrapped via the GNU `env` command instead of tmux's `-e` flag
// because tmux's `-e` is silently overridden for PATH (verified empirically:
// tmux merges PATH from the client and ignores the per-pane override). All
// other env vars still go through `-e`, which tmux honors normally.
func (a *Adapter) NewWindowWithEnv(session, name, dir, command string, args []string, env map[string]string) (*WindowInfo, error) {
	tmuxArgs := []string{
		"new-window",
		"-t", session,
		"-n", name,
		"-P", "-F", windowFormat,
	}
	if dir != "" {
		tmuxArgs = append(tmuxArgs, "-c", dir)
	}
	// Split env into PATH (needs `env` wrapping) and the rest (tmux -e).
	pathVal, hasPath := env["PATH"]
	for k, v := range env {
		if k == "PATH" {
			continue
		}
		tmuxArgs = append(tmuxArgs, "-e", k+"="+v)
	}
	if command != "" {
		// Build the shell-quoted command chain. When PATH override is requested
		// we prepend `env PATH='<val>' <cmd> <args...>` so the resulting
		// execution chain is: tmux → bash -c → env → claude.
		var parts []string
		if hasPath {
			parts = append(parts, "env", "PATH="+pathVal)
		}
		parts = append(parts, command)
		parts = append(parts, args...)
		shellCmd := ""
		for i, p := range parts {
			if i > 0 {
				shellCmd += " "
			}
			shellCmd += shellSingleQuote(p)
		}
		tmuxArgs = append(tmuxArgs, shellCmd)
	}
	out, err := a.run(tmuxArgs...)
	if err != nil {
		return nil, fmt.Errorf("new-window: %w", err)
	}
	p := NewParser()
	return p.ParseWindowInfo(out)
}

// KillWindow kills the window identified by target.
func (a *Adapter) KillWindow(target string) error {
	if _, err := a.run("kill-window", "-t", target); err != nil {
		return fmt.Errorf("kill-window %s: %w", target, err)
	}
	return nil
}

// SendKeys sends text to the target pane using literal mode, then presses Enter.
//
// The Enter is delayed after the literal text. Claude Code's TUI ingests
// keystrokes asynchronously; on a long or multi-line message the Enter would
// otherwise race ahead of the still-arriving text and the message is left
// unsent in the input box (looks like "no response"). The delay scales with the
// message so short single-line sends stay snappy.
func (a *Adapter) SendKeys(target, text string) error {
	if _, err := a.run("send-keys", "-t", target, "-l", text); err != nil {
		return fmt.Errorf("send-keys text %s: %w", target, err)
	}
	time.Sleep(submitDelay(text))
	if _, err := a.run("send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("send-keys enter %s: %w", target, err)
	}
	return nil
}

// submitDelay returns how long to wait between the literal text and the Enter,
// scaled by line count and length, capped at 1s.
func submitDelay(text string) time.Duration {
	d := 80*time.Millisecond +
		time.Duration(strings.Count(text, "\n"))*40*time.Millisecond +
		time.Duration(len(text)/200)*20*time.Millisecond
	if d > time.Second {
		d = time.Second
	}
	return d
}

// ListWindows returns all windows in the given session.
func (a *Adapter) ListWindows(session string) ([]WindowInfo, error) {
	out, err := a.run("list-windows", "-t", session, "-F", windowFormat)
	if err != nil {
		return nil, fmt.Errorf("list-windows %s: %w", session, err)
	}
	p := NewParser()
	return p.ParseListWindows(out)
}

// CapturePane returns the scrollback content of the target pane (last 500 lines).
func (a *Adapter) CapturePane(target string) (string, error) {
	out, err := a.run("capture-pane", "-t", target, "-p", "-S", "-500")
	if err != nil {
		return "", fmt.Errorf("capture-pane %s: %w", target, err)
	}
	return out, nil
}
