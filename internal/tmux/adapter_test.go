package tmux

import (
	"testing"
)

func TestParseListWindows(t *testing.T) {
	data := []byte("tgcc\t0\tapi-refactor\t@0\t12345\ntgcc\t1\tweb-bugfix\t@1\t12346\n")

	windows := ParseListWindows(data)

	if len(windows) != 2 {
		t.Fatalf("expected 2 windows, got %d", len(windows))
	}

	w1 := windows[0]
	if w1.SessionName != "tgcc" {
		t.Errorf("expected session tgcc, got %s", w1.SessionName)
	}
	if w1.WindowName != "api-refactor" {
		t.Errorf("expected api-refactor, got %s", w1.WindowName)
	}
	if w1.PanePID != 12345 {
		t.Errorf("expected PID 12345, got %d", w1.PanePID)
	}
	if !w1.PaneAlive {
		t.Error("expected pane alive")
	}

	w2 := windows[1]
	if w2.WindowName != "web-bugfix" {
		t.Errorf("expected web-bugfix, got %s", w2.WindowName)
	}
	if w2.PanePID != 12346 {
		t.Errorf("expected PID 12346, got %d", w2.PanePID)
	}
}

func TestParseListWindowsEmpty(t *testing.T) {
	windows := ParseListWindows([]byte(""))
	if len(windows) != 0 {
		t.Errorf("expected 0 windows, got %d", len(windows))
	}

	windows = ParseListWindows(nil)
	if len(windows) != 0 {
		t.Errorf("expected 0 windows, got %d", len(windows))
	}
}

func TestParseListWindowsDeadProcess(t *testing.T) {
	// A window with PID 0 means process is not running
	data := []byte("tgcc\t0\tdead-window\t@0\t0\n")

	windows := ParseListWindows(data)

	if len(windows) != 1 {
		t.Fatalf("expected 1 window, got %d", len(windows))
	}

	if windows[0].PaneAlive {
		t.Error("expected dead pane (PID 0)")
	}
}

func TestParseListWindowsTruncated(t *testing.T) {
	// Truncated line
	data := []byte("tgcc\t0\tpartial\n")

	windows := ParseListWindows(data)

	// Should be skipped because fewer than 5 fields
	if len(windows) != 0 {
		t.Errorf("expected 0 windows (truncated), got %d", len(windows))
	}
}
