package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/testutil"
	"github.com/jaekwon-park/tgcc/internal/tmux"
)

// writeFakeTmux writes a stub tmux binary that fails any "send-keys"
// invocation the way real tmux does when the target window is gone, and
// succeeds (no-op) for everything else. This reproduces ADM-1178/1253: a
// resumed claude process that exited immediately, leaving the DB pointing at
// a tmux window that no longer exists.
func writeFakeTmux(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-tmux.sh")
	script := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "send-keys" ]; then
    echo "can't find window: @12" >&2
    exit 1
  fi
done
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	return path
}

func newTestManagerWithDeadWindow(t *testing.T) (*Manager, *store.Store, string) {
	t.Helper()
	st := testutil.Store(t, 11)
	topic, err := st.InsertTopic(-1, 1, "test-topic")
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "dead-window-session"
	if err := st.InsertSession(&store.Session{
		ID:             sessionID,
		TopicID:        topic.ID,
		TmuxSession:    "tgcc",
		TmuxWindow:     "@12",
		WorkspacePath:  t.TempDir(),
		Status:         "active",
		CreatedAt:      store.CurrentTimeMs(),
		LastActivityAt: store.CurrentTimeMs(),
	}); err != nil {
		t.Fatal(err)
	}

	adapter := tmux.NewAdapter(writeFakeTmux(t), "tgcc", nil)
	mgr := NewManager(st, adapter, nil, nil, nil, "tgcc", "claude", t.TempDir(), nil, nil)
	return mgr, st, sessionID
}

// TestForwardMessage_WindowGone_MarksSessionFailed reproduces ADM-1178/1253:
// send-keys against a session whose tmux window no longer exists must (1)
// return an error wrapping ErrWindowGone so router.Route can retry via a
// fresh spawn, and (2) mark the session failed in the DB instead of leaving
// it "active" forever on a dead window.
func TestForwardMessage_WindowGone_MarksSessionFailed(t *testing.T) {
	mgr, st, sessionID := newTestManagerWithDeadWindow(t)

	err := mgr.ForwardMessage(context.Background(), sessionID, "hello")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrWindowGone) {
		t.Fatalf("expected error to wrap ErrWindowGone, got: %v", err)
	}

	sess, serr := st.SessionByID(sessionID)
	if serr != nil {
		t.Fatalf("reload session: %v", serr)
	}
	if sess.Status != string(StatusFailed) {
		t.Fatalf("expected session status %q after dead-window send-keys, got %q", StatusFailed, sess.Status)
	}
}
