package session

import (
	"testing"

	"github.com/jaekwon-park/tgcc/internal/tmux"
)

func TestBuildWindowMap(t *testing.T) {
	windows := []tmux.TmuxWindowInfo{
		{SessionName: "tgcc", WindowName: "api-refactor", WindowID: "@0", PanePID: 12345, PaneAlive: true},
		{SessionName: "tgcc", WindowName: "web-bugfix", WindowID: "@1", PanePID: 12346, PaneAlive: true},
	}

	m := buildWindowMap(windows)

	// Test lookup by session:window_name
	if w, ok := m["tgcc:api-refactor"]; !ok {
		t.Error("expected tgcc:api-refactor in map")
	} else if w.PanePID != 12345 {
		t.Errorf("expected PID 12345, got %d", w.PanePID)
	}

	// Test lookup by window_id
	if w, ok := m["@1"]; !ok {
		t.Error("expected @1 in map")
	} else if w.WindowName != "web-bugfix" {
		t.Errorf("expected web-bugfix, got %s", w.WindowName)
	}

	// Test non-existent key
	if _, ok := m["tgcc:nonexistent"]; ok {
		t.Error("expected nonexistent key to be missing")
	}
}

func TestBuildWindowMapEmpty(t *testing.T) {
	m := buildWindowMap(nil)
	if len(m) != 0 {
		t.Errorf("expected empty map, got %d entries", len(m))
	}

	m = buildWindowMap([]tmux.TmuxWindowInfo{})
	if len(m) != 0 {
		t.Errorf("expected empty map, got %d entries", len(m))
	}
}
