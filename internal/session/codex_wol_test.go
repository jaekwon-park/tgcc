package session

import (
	"context"
	"encoding/json"
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

func TestIsSSHConnectFailure(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"원격 호스트 flcl@10.1.1.104 연결 실패: exit status 255: ssh: connect to host 10.1.1.104 port 22: Connection timed out", true},
		{"ssh: connect to host 10.1.1.104 port 22: Connection refused", true},
		{"dial tcp 10.1.1.104:22: connect: no route to host", true},
		{"Connection Timed Out", true},
		{"codex exit: exit status 1: something else", false},
		{"원격 Codex가 완료된 턴/스레드 없이 종료되었습니다", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = fmt.Errorf("%s", c.msg)
		}
		if got := isSSHConnectFailure(err); got != c.want {
			t.Errorf("isSSHConnectFailure(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func TestWolBroadcast(t *testing.T) {
	cases := []struct {
		relay string
		want  string
	}{
		{"insainty@10.1.1.21", "10.1.1.255"},
		{"10.1.1.21", "10.1.1.255"},
		{"insainty@relay-host", ""},
		{"relay-host", ""},
		{"", ""},
		{"user@2001:db8::1", ""},
	}
	for _, c := range cases {
		if got := wolBroadcast(c.relay); got != c.want {
			t.Errorf("wolBroadcast(%q) = %q, want %q", c.relay, got, c.want)
		}
	}
}

func TestWolPythonScriptAndCommand(t *testing.T) {
	script := wolPythonScript("a0:ad:9f:cd:5e:8a", "10.1.1.255")
	if strings.Contains(script, "'") {
		t.Fatalf("python script must not contain single quotes: %s", script)
	}
	if !strings.Contains(script, `"a0:ad:9f:cd:5e:8a"`) {
		t.Fatalf("MAC not embedded: %s", script)
	}
	if !strings.Contains(script, "10.1.1.255") || !strings.Contains(script, "255.255.255.255") {
		t.Fatalf("broadcast targets missing: %s", script)
	}

	cmd := buildWOLRemoteCommand("a0:ad:9f:cd:5e:8a", "10.1.1.255")
	if !strings.Contains(cmd, "wakeonlan") {
		t.Fatalf("wakeonlan not preferred: %s", cmd)
	}
	if !strings.Contains(cmd, "python3 -c") {
		t.Fatalf("python fallback missing: %s", cmd)
	}
	if !strings.Contains(cmd, "a0:ad:9f:cd:5e:8a") {
		t.Fatalf("MAC missing from relay command: %s", cmd)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(""); got != "''" {
		t.Fatalf("empty: %q", got)
	}
	if got := shellQuote("a0:ad:9f:cd:5e:8a"); got != "'a0:ad:9f:cd:5e:8a'" {
		t.Fatalf("plain: %q", got)
	}
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("with quote: %q", got)
	}
}

// TestWOLSSHProcess acts as a fake remote `ssh` binary. It distinguishes two
// invocations by the trailing remote command:
//   - WOL relay calls (contain "wakeonlan"/"python3 -c"): log argv and exit 0.
//   - codex exec attempts (contain "codex"): fail the first attempt with a
//     "Connection timed out" stderr + exit 255, then emit a successful JSONL
//     stream on later attempts.
func TestWOLSSHProcess(t *testing.T) {
	if os.Getenv("TGCC_WOL_HELPER") != "1" {
		return
	}
	args := os.Args
	for index, arg := range args {
		if arg == "--" {
			args = args[index+1:]
			break
		}
	}
	remote := args[len(args)-1]

	if strings.Contains(remote, "wakeonlan") || strings.Contains(remote, "python3 -c") {
		if logPath := os.Getenv("TGCC_WOL_LOG"); logPath != "" {
			f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err == nil {
				_ = json.NewEncoder(f).Encode(codexInvocation{Args: args, Prompt: remote})
				_ = f.Close()
			}
		}
		os.Exit(0)
	}

	counterPath := os.Getenv("TGCC_WOL_COUNTER")
	data, _ := os.ReadFile(counterPath)
	n, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	n++
	_ = os.WriteFile(counterPath, []byte(strconv.Itoa(n)), 0600)
	if n == 1 {
		fmt.Fprintln(os.Stderr, "ssh: connect to host 10.1.1.104 port 22: Connection timed out")
		os.Exit(255)
	}
	fmt.Println(`{"type":"thread.started","thread_id":"wol-thread"}`)
	fmt.Println(`{"type":"turn.started"}`)
	fmt.Println(`{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"woke-ok"}}`)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	os.Exit(0)
}

func TestCodexWOLWakeRetry(t *testing.T) {
	st := testutil.Store(t, 11)
	topic, err := st.InsertTopic(-1, 1, "codex-wol")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SyncTopicCLI(topic.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTopicRemote(topic.ID, "flcl@10.1.1.104", `C:\Users\FLCL\re-analysis`); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTopicWOL(topic.ID, "a0:ad:9f:cd:5e:8a", "insainty@10.1.1.21"); err != nil {
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

	logPath := filepath.Join(t.TempDir(), "wol-calls.jsonl")
	counterPath := filepath.Join(t.TempDir(), "wol-counter")
	if err := os.WriteFile(counterPath, []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := NewManager(st, nil, logger, nil, nil, "", "", "", nil, nil)
	capture := &codexCapture{}
	manager.codex = newCodexRunner(st, "codex", map[string]string{
		"TGCC_WOL_HELPER":  "1",
		"TGCC_WOL_LOG":     logPath,
		"TGCC_WOL_COUNTER": counterPath,
	}, capture, capture, logger)
	manager.codex.sshBin = sshBin
	manager.codex.retryDelay = time.Millisecond
	manager.codex.wolRetryDelay = time.Millisecond
	manager.codex.wolRetryAttempts = 3
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
	if !strings.Contains(capture.texts(), "woke-ok") {
		t.Fatalf("remote agent message not relayed: %s", capture.texts())
	}
	if !strings.Contains(capture.texts(), "깨우는 중") {
		t.Fatalf("wake notice not sent: %s", capture.texts())
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "a0:ad:9f:cd:5e:8a") {
		t.Fatalf("WOL relay invocation missing MAC: %s", data)
	}
	if !strings.Contains(string(data), "insainty@10.1.1.21") {
		t.Fatalf("WOL relay host not used: %s", data)
	}
}

// TestCodexWOLDisabledFailsImmediately verifies no regression: a remote topic
// without wol config still fails immediately on a connection error (no retry,
// no wake notice).
func TestCodexWOLDisabledFailsImmediately(t *testing.T) {
	st := testutil.Store(t, 11)
	topic, err := st.InsertTopic(-1, 1, "codex-no-wol")
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
	counterPath := filepath.Join(t.TempDir(), "wol-counter")
	if err := os.WriteFile(counterPath, []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := NewManager(st, nil, logger, nil, nil, "", "", "", nil, nil)
	capture := &codexCapture{}
	manager.codex = newCodexRunner(st, "codex", map[string]string{
		"TGCC_WOL_HELPER":  "1",
		"TGCC_WOL_COUNTER": counterPath,
	}, capture, capture, logger)
	manager.codex.sshBin = sshBin
	manager.codex.retryDelay = time.Millisecond
	manager.codex.wolRetryDelay = time.Millisecond
	manager.codex.wolRetryAttempts = 3
	t.Cleanup(manager.Shutdown)

	sess, err := manager.Spawn(context.Background(), topic.ID, `C:\Users\FLCL\re-analysis`, -1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), sess.ID, "analyze"); err != nil {
		t.Fatal(err)
	}
	sess = waitCodex(t, manager, sess.ID)
	if sess.Status != "failed" {
		t.Fatalf("expected failed status without WOL config, got %q", sess.Status)
	}
	if strings.Contains(capture.texts(), "깨우는 중") {
		t.Fatalf("wake notice must not be sent without WOL config: %s", capture.texts())
	}
}
