package session

import (
	"database/sql"

	"github.com/jaekwon-park/tgcc/internal/store"
)

// newTestSessionRow creates a test SessionRow with default values.
func newTestSessionRow() store.SessionRow {
	return store.SessionRow{
		ID:              "test-session-id",
		TopicID:         1,
		TmuxSession:     "tgcc",
		TmuxWindow:      "test-window",
		WorkspacePath:   "/tmp/test-workspace",
		ClaudeSessionID: sql.NullString{Valid: false},
		PID:             sql.NullInt64{Valid: false},
		Status:          "active",
		LastActivityAt:  0,
		CreatedAt:       0,
	}
}
