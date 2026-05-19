package session

import (
	"testing"
	"time"
)

func TestNewSupervisorDefaults(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	if sup.maxRetries != 3 {
		t.Errorf("expected maxRetries=3, got %d", sup.maxRetries)
	}
	if sup.tick != 5*time.Second {
		t.Errorf("expected tick=5s, got %v", sup.tick)
	}
	if len(sup.retryCounts) != 0 {
		t.Errorf("expected empty retryCounts, got %d entries", len(sup.retryCounts))
	}
}

func TestNewSupervisorCustom(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{
		MaxRetries: 5,
		Tick:       10 * time.Second,
	})
	if sup.maxRetries != 5 {
		t.Errorf("expected maxRetries=5, got %d", sup.maxRetries)
	}
	if sup.tick != 10*time.Second {
		t.Errorf("expected tick=10s, got %v", sup.tick)
	}
}

func TestResetRetries(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})
	sup.retryCounts["session-1"] = 2
	sup.retryCounts["session-2"] = 1

	sup.ResetRetries("session-1")

	if _, ok := sup.retryCounts["session-1"]; ok {
		t.Error("expected session-1 to be reset")
	}
	if count, ok := sup.retryCounts["session-2"]; !ok || count != 1 {
		t.Error("expected session-2 to remain with count 1")
	}
}

func TestBuildResumeCommand(t *testing.T) {
	sup := NewSupervisor(SupervisorConfig{})

	// Test with claude_session_id
	row := newTestSessionRow()
	row.ClaudeSessionID.String = "abc-123-session"
	row.ClaudeSessionID.Valid = true
	cmd := sup.buildResumeCommand(row)
	if cmd != "claude --resume abc-123-session" {
		t.Errorf("expected resume command, got %q", cmd)
	}

	// Test without claude_session_id
	row2 := newTestSessionRow()
	cmd2 := sup.buildResumeCommand(row2)
	if cmd2 != "claude" {
		t.Errorf("expected fallback command, got %q", cmd2)
	}
}
