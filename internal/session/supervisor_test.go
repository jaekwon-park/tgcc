package session

import (
	"testing"
	"time"

	"github.com/jaekwon-park/tgcc/internal/config"
)

func TestNewSupervisorNonZeroInterval(t *testing.T) {
	sup := NewSupervisor(nil, nil, 10*time.Second, config.DefaultContextConfig(), nil, nil, nil)
	if sup.interval != 10*time.Second {
		t.Errorf("expected interval=10s, got %v", sup.interval)
	}
}

func TestNewSupervisorDefaultInterval(t *testing.T) {
	sup := NewSupervisor(nil, nil, 0, config.DefaultContextConfig(), nil, nil, nil)
	if sup.interval != defaultSupervisorInterval {
		t.Errorf("expected default interval=%v, got %v", defaultSupervisorInterval, sup.interval)
	}
}
