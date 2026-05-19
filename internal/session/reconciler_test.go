package session

import (
	"testing"

	"github.com/jaekwon-park/tgcc/internal/tmux"
)

func TestWindowAliveMap(t *testing.T) {
	windows := []tmux.WindowInfo{
		{ID: "@0", Name: "api-refactor", PID: 12345},
		{ID: "@1", Name: "web-bugfix", PID: 12346},
	}

	alive := make(map[string]bool, len(windows))
	for _, w := range windows {
		alive[w.ID] = true
	}

	if !alive["@0"] {
		t.Error("expected @0 in alive map")
	}
	if !alive["@1"] {
		t.Error("expected @1 in alive map")
	}
	if alive["@2"] {
		t.Error("expected @2 absent from alive map")
	}
}
