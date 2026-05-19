// Package tmux provides a thin adapter for tmux CLI commands.
package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// TmuxWindowInfo holds parsed information about a tmux window.
type TmuxWindowInfo struct {
	SessionName string
	WindowIndex string
	WindowName  string
	WindowID    string
	PanePID     int
	PaneAlive   bool
}

// Adapter wraps tmux CLI operations.
type Adapter struct {
	binary string
}

// NewAdapter creates a new tmux adapter.
func NewAdapter(binary string) *Adapter {
	if binary == "" {
		binary = "tmux"
	}
	return &Adapter{binary: binary}
}

// ListWindows returns all windows from the tmux server.
func (a *Adapter) ListWindows(ctx context.Context, format string) ([]byte, error) {
	if format == "" {
		format = "#{session_name}\t#{window_index}\t#{window_name}\t#{window_id}\t#{pane_pid}"
	}
	cmd := exec.CommandContext(ctx, a.binary, "list-windows", "-F", format)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("tmux list-windows: %w (stderr: check tmux server)", err)
	}
	return out, nil
}

// ParseListWindows parses tmux list-windows output into structured info.
// Expected format: session_name\twindow_index\twindow_name\twindow_id\tpane_pid
func ParseListWindows(data []byte) []TmuxWindowInfo {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var windows []TmuxWindowInfo
	for _, line := range lines {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		pid := 0
		alive := false
		if _, err := fmt.Sscanf(fields[4], "%d", &pid); err == nil && pid > 0 {
			alive = true
		}
		windows = append(windows, TmuxWindowInfo{
			SessionName: fields[0],
			WindowIndex: fields[1],
			WindowName:  fields[2],
			WindowID:    fields[3],
			PanePID:     pid,
			PaneAlive:   alive,
		})
	}
	return windows
}

// IsWindowAlive checks if a specific tmux window exists and has a running process.
func (a *Adapter) IsWindowAlive(ctx context.Context, sessionName, windowTarget string) (bool, int, error) {
	// Check if pane exists and get its PID
	target := fmt.Sprintf("%s:%s", sessionName, windowTarget)
	cmd := exec.CommandContext(ctx, a.binary, "display-message", "-t", target, "-p", "#{pane_pid}")
	out, err := cmd.Output()
	if err != nil {
		// tmux returns non-zero if window/target doesn't exist
		return false, 0, nil
	}

	pid := 0
	pidStr := strings.TrimSpace(string(out))
	if _, err := fmt.Sscanf(pidStr, "%d", &pid); err != nil || pid <= 0 {
		return false, 0, nil
	}

	// Check if the process is actually still running
	if err := exec.CommandContext(ctx, "kill", "-0", pidStr).Run(); err != nil {
		return false, pid, nil
	}

	return true, pid, nil
}

// KillWindow kills a tmux window.
func (a *Adapter) KillWindow(ctx context.Context, sessionName, windowTarget string) error {
	target := fmt.Sprintf("%s:%s", sessionName, windowTarget)
	cmd := exec.CommandContext(ctx, a.binary, "kill-window", "-t", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux kill-window %s: %w (output: %s)", target, err, string(out))
	}
	return nil
}

// NewWindow creates a new tmux window and runs the given command.
func (a *Adapter) NewWindow(ctx context.Context, sessionName, windowName, workDir, command string) error {
	// tmux new-window -t <session> -n <window-name> -c <dir> <command>
	cmd := exec.CommandContext(ctx, a.binary,
		"new-window",
		"-t", sessionName,
		"-n", windowName,
		"-c", workDir,
		command,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-window: %w (output: %s)", err, string(out))
	}
	return nil
}

// SendKeys sends keystrokes to a tmux window.
func (a *Adapter) SendKeys(ctx context.Context, sessionName, windowTarget, keys string) error {
	target := fmt.Sprintf("%s:%s", sessionName, windowTarget)
	cmd := exec.CommandContext(ctx, a.binary, "send-keys", "-t", target, keys)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux send-keys: %w (output: %s)", err, string(out))
	}
	return nil
}
