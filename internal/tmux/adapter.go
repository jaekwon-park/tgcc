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

// NewWindow creates a new window in the given session and returns its WindowInfo.
func (a *Adapter) NewWindow(session, name, cmd string) (*WindowInfo, error) {
	args := []string{
		"new-window",
		"-t", session,
		"-n", name,
		"-P", "-F", windowFormat,
	}
	if cmd != "" {
		args = append(args, cmd)
	}
	out, err := a.run(args...)
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
func (a *Adapter) SendKeys(target, text string) error {
	if _, err := a.run("send-keys", "-t", target, "-l", text); err != nil {
		return fmt.Errorf("send-keys text %s: %w", target, err)
	}
	if _, err := a.run("send-keys", "-t", target, "Enter"); err != nil {
		return fmt.Errorf("send-keys enter %s: %w", target, err)
	}
	return nil
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
