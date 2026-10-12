package tmux

import (
	"os/exec"
	"strings"
	"testing"
)

// newTestAdapter starts an isolated tmux server (own socket dir) with a single
// session/window and returns an Adapter plus a cleanup func. Skips the test if
// tmux isn't available.
func newTestAdapter(t *testing.T) (a *Adapter, target string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}

	sockDir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", sockDir)

	session := "tgcc-adapter-test"
	cmd := exec.Command("tmux", "new-session", "-d", "-s", session, "-x", "80", "-y", "24")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tmux new-session: %v (%s)", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("tmux", "kill-server").Run()
	})

	return NewAdapter("", session, nil), session + ":0"
}

func TestSendKeys_LeadingDashText(t *testing.T) {
	a, target := newTestAdapter(t)

	// Text starting with "-" must not be misparsed as a tmux send-keys flag
	// (regression for "invalid flag -" when forwarding Telegram messages that
	// start with a hyphen, e.g. a markdown list item).
	const msg = "-세션상태 확인해줘"
	if err := a.SendKeys(target, msg); err != nil {
		t.Fatalf("SendKeys with leading-dash text failed: %v", err)
	}

	out, err := a.run("capture-pane", "-t", target, "-p")
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	if !strings.Contains(out, msg) {
		t.Fatalf("pane output does not contain sent text.\ngot:\n%s", out)
	}
}

func TestSendKeys_PlainText(t *testing.T) {
	a, target := newTestAdapter(t)

	const msg = "hello world"
	if err := a.SendKeys(target, msg); err != nil {
		t.Fatalf("SendKeys failed: %v", err)
	}

	out, err := a.run("capture-pane", "-t", target, "-p")
	if err != nil {
		t.Fatalf("capture-pane: %v", err)
	}
	if !strings.Contains(out, msg) {
		t.Fatalf("pane output does not contain sent text.\ngot:\n%s", out)
	}
}
