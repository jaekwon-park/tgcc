package session

import (
	"testing"
)

func TestValidTransitions(t *testing.T) {
	tests := []struct {
		from   string
		to     string
		expect bool
	}{
		// Happy paths
		{"pending", "spawning", true},
		{"spawning", "active", true},
		{"active", "idle", true},
		{"active", "crashed", true},
		{"active", "stopping", true},
		{"idle", "active", true},
		{"idle", "crashed", true},
		{"idle", "stopping", true},
		{"crashed", "resuming", true},
		{"resuming", "active", true},
		{"resuming", "failed", true},
		{"stopping", "stopped", true},
		{"stopping", "failed", true},

		// Invalid transitions
		{"active", "failed", false},
		{"active", "resuming", false},
		{"crashed", "active", false},
		{"failed", "active", false},
		{"stopped", "active", false},
		{"pending", "active", false},
		{"idle", "spawning", false},
	}

	for _, tt := range tests {
		t.Run(tt.from+"->"+tt.to, func(t *testing.T) {
			got := CanTransition(tt.from, tt.to)
			if got != tt.expect {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.expect)
			}
		})
	}
}

func TestIsActiveStatus(t *testing.T) {
	if !IsActiveStatus("active") {
		t.Error("expected active to be active status")
	}
	if !IsActiveStatus("idle") {
		t.Error("expected idle to be active status")
	}
	if !IsActiveStatus("resuming") {
		t.Error("expected resuming to be active status")
	}
	if IsActiveStatus("crashed") {
		t.Error("expected crashed to NOT be active status")
	}
	if IsActiveStatus("failed") {
		t.Error("expected failed to NOT be active status")
	}
}

func TestIsTerminalStatus(t *testing.T) {
	if !IsTerminalStatus("failed") {
		t.Error("expected failed to be terminal")
	}
	if !IsTerminalStatus("stopped") {
		t.Error("expected stopped to be terminal")
	}
	if IsTerminalStatus("active") {
		t.Error("expected active to NOT be terminal")
	}
	if IsTerminalStatus("crashed") {
		t.Error("expected crashed to NOT be terminal")
	}
}
