package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jaekwon-park/tgcc/internal/testutil"
)

func TestIsSSHConnectionFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "timeout",
			err:  fmt.Errorf("원격 호스트 flcl@10.1.1.104 연결 실패: exit status 255: ssh: connect to host 10.1.1.104 port 22: Connection timed out"),
			want: true,
		},
		{
			name: "refused",
			err:  fmt.Errorf("원격 호스트 flcl@10.1.1.104 연결 실패: exit status 255: ssh: connect to host 10.1.1.104 port 22: Connection refused"),
			want: true,
		},
		{
			name: "auth-failure",
			err:  fmt.Errorf("원격 호스트 flcl@10.1.1.104 연결 실패: exit status 255: Permission denied (publickey)"),
			want: false,
		},
		{
			name: "codex-error",
			err:  fmt.Errorf("원격 Codex가 완료된 턴/스레드 없이 종료되었습니다"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSSHConnectionFailure(c.err); got != c.want {
				t.Fatalf("isSSHConnectionFailure(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestWolMagicPacketScript(t *testing.T) {
	script := wolMagicPacketScript("a0:ad:9f:cd:5e:8a")
	for _, want := range []string{
		`mac="a0:ad:9f:cd:5e:8a"`,
		`packet=b"\xff"*6 + mac_bytes*16`,
		`("10.1.1.255", 9)`,
		`("255.255.255.255", 7)`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	// No single quotes, so it wraps cleanly in POSIX single-quote quoting.
	if strings.Contains(script, "'") {
		t.Fatalf("script must not contain single quotes for shell quoting:\n%s", script)
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello", "'hello'"},
		{"a'b", `'a'\''b'`},
		{"", "''"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Fatalf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestWOLSSHProcess acts as a fake remote `ssh` binary for the WOL retry test.
// Calls targeting the relay host (wolRelayHost) record themselves and succeed.
// Calls targeting the codex host fail with "Connection timed out" the first
// two times (simulating a powered-off Windows PC), then emit a successful
// codex JSONL stream.
func TestWOLSSHProcess(t *testing.T) {
	if os.Getenv("TGCC_WOL_SSH_HELPER") != "1" {
		return
	}
	args := os.Args
	for index, arg := range args {
		if arg == "--" {
			args = args[index+1:]
			break
		}
	}
	joined := strings.Join(args, " ")

	if strings.Contains(joined, wolRelayHost) {
		if relayLog := os.Getenv("TGCC_WOL_RELAY_LOG"); relayLog != "" {
			if log, err := os.OpenFile(relayLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				_, _ = fmt.Fprintln(log, "wol-relay")
				_ = log.Close()
			}
		}
		os.Exit(0)
	}

	if codexLog := os.Getenv("TGCC_WOL_CODEX_LOG"); codexLog != "" {
		if log, err := os.OpenFile(codexLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
			_, _ = fmt.Fprintln(log, "codex")
			_ = log.Close()
		}
	}

	counterPath := os.Getenv("TGCC_WOL_COUNTER")
	data, _ := os.ReadFile(counterPath)
	attempt, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	_ = os.WriteFile(counterPath, []byte(strconv.Itoa(attempt+1)), 0600)

	if attempt < 2 {
		fmt.Fprintln(os.Stderr, "ssh: connect to host 10.1.1.104 port 22: Connection timed out")
		os.Exit(255)
	}

	fmt.Println(`{"type":"thread.started","thread_id":"wol-thread"}`)
	fmt.Println(`{"type":"turn.started"}`)
	fmt.Println(`{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"wol-ok"}}`)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	os.Exit(0)
}

func TestCodexRemoteWOLRetry(t *testing.T) {
	st := testutil.Store(t, 10)
	topic, err := st.InsertTopic(-1, 1, "codex-remote-wol")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SyncTopicCLI(topic.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTopicRemote(topic.ID, "flcl@10.1.1.104", `C:\Users\FLCL\re-analysis`); err != nil {
		t.Fatal(err)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sshBin := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestWOLSSHProcess$ -- \"$@\"\n"
	if err := os.WriteFile(sshBin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	counterPath := filepath.Join(t.TempDir(), "counter")
	relayLog := filepath.Join(t.TempDir(), "relay.log")
	codexLog := filepath.Join(t.TempDir(), "codex.log")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := NewManager(st, nil, logger, nil, nil, "", "", "", nil, nil)
	capture := &codexCapture{}
	manager.codex = newCodexRunner(st, "codex", map[string]string{
		"TGCC_WOL_SSH_HELPER": "1",
		"TGCC_WOL_COUNTER":    counterPath,
		"TGCC_WOL_RELAY_LOG":  relayLog,
		"TGCC_WOL_CODEX_LOG":  codexLog,
	}, capture, capture, logger)
	manager.codex.sshBin = sshBin
	manager.codex.wolInterval = time.Millisecond
	t.Cleanup(manager.Shutdown)

	sess, err := manager.Spawn(context.Background(), topic.ID, `C:\Users\FLCL\re-analysis`, -1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), sess.ID, "analyze this binary"); err != nil {
		t.Fatal(err)
	}
	sess = waitCodex(t, manager, sess.ID)
	if sess.Status != "idle" || sess.CodexThreadID != "wol-thread" {
		t.Fatalf("bad session after WOL retry: %+v", sess)
	}
	if !strings.Contains(capture.texts(), "wol-ok") {
		t.Fatalf("remote agent message not relayed: %s", capture.texts())
	}
	if !strings.Contains(capture.texts(), "깨우는 중") {
		t.Fatalf("WOL wake notice not sent: %s", capture.texts())
	}

	data, err := os.ReadFile(relayLog)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		codexData, _ := os.ReadFile(codexLog)
		counterData, _ := os.ReadFile(counterPath)
		t.Fatalf("expected exactly 1 WOL relay call, got %d: %q\ncodex calls: %q\ncounter: %q", len(lines), data, codexData, counterData)
	}
}
