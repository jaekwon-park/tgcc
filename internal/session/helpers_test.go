package session

import (
	"github.com/jaekwon-park/tgcc/internal/store"
)

// newTestSessionRow creates a test Session with default values.
func newTestSessionRow() store.Session {
	return store.Session{
		ID:            "test-session-id",
		TopicID:       1,
		TmuxSession:   "tgcc",
		TmuxWindow:    "test-window",
		WorkspacePath: "/tmp/test-workspace",
		Status:        "active",
	}
}
