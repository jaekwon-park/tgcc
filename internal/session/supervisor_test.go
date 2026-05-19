package session

import (
	"testing"
	"time"
)

func TestNewSupervisorNonZeroInterval(t *testing.T) {
	sup := NewSupervisor(nil, nil, 10*time.Second)
	if sup.interval != 10*time.Second {
		t.Errorf("expected interval=10s, got %v", sup.interval)
	}
}

func TestNewSupervisorDefaultInterval(t *testing.T) {
	sup := NewSupervisor(nil, nil, 0)
	if sup.interval != defaultSupervisorInterval {
		t.Errorf("expected default interval=%v, got %v", defaultSupervisorInterval, sup.interval)
	}
}
