package tmux

import (
	"fmt"
	"strconv"
	"strings"
)

// windowFormat is the -F format string shared by NewWindow and ListWindows.
// Fields are separated by TAB to avoid collisions with colons in window names.
// Layout: <name>\t<id>\t<pid>
const windowFormat = "#{window_name}\t#{window_id}\t#{pane_pid}"

// WindowInfo holds metadata about a tmux window.
type WindowInfo struct {
	Name string
	ID   string
	PID  int
}

// Parser parses tmux command output.
type Parser struct{}

// NewParser creates a new Parser.
func NewParser() *Parser {
	return &Parser{}
}

// ParseWindowInfo parses a single line produced by new-window -P -F windowFormat.
func (p *Parser) ParseWindowInfo(line string) (*WindowInfo, error) {
	line = strings.TrimRight(line, "\n")
	parts := strings.Split(line, "\t")
	if len(parts) != 3 {
		return nil, fmt.Errorf("parse window info: expected 3 tab-separated fields, got %d in %q", len(parts), line)
	}
	pid, err := strconv.Atoi(parts[2])
	if err != nil {
		return nil, fmt.Errorf("parse window pid %q: %w", parts[2], err)
	}
	return &WindowInfo{
		Name: parts[0],
		ID:   parts[1],
		PID:  pid,
	}, nil
}

// ParseListWindows parses the output of list-windows -F windowFormat.
func (p *Parser) ParseListWindows(out string) ([]WindowInfo, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	windows := make([]WindowInfo, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		wi, err := p.ParseWindowInfo(line)
		if err != nil {
			return nil, err
		}
		windows = append(windows, *wi)
	}
	return windows, nil
}
