package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaekwon-park/tgcc/internal/testutil"
)

func TestCmdQuote(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"exec", "exec"},
		{"--model", "--model"},
		{"gpt-5.5", "gpt-5.5"},
		{"", `""`},
		{`C:\Users\FLCL\re-analysis`, `C:\Users\FLCL\re-analysis`},
		{"with space", `"with space"`},
		{`a"b`, `"a""b"`},
		{"a&b", `"a&b"`},
		{"a|b", `"a|b"`},
	}
	for _, c := range cases {
		if got := cmdQuote(c.in); got != c.want {
			t.Errorf("cmdQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildRemoteCommandLine(t *testing.T) {
	args := []string{"exec", "--json", "--model", "gpt-5.5", "-"}

	got := buildRemoteCommandLine(`C:\Users\FLCL\re-analysis`, args)
	want := `codex exec --cd C:\Users\FLCL\re-analysis --json --model gpt-5.5 -`
	if got != want {
		t.Fatalf("plain: got %q, want %q", got, want)
	}

	// Workspace containing spaces must be double-quoted for cmd.exe.
	got = buildRemoteCommandLine(`C:\Users\FLCL\re analysis`, args)
	want = `codex exec --cd "C:\Users\FLCL\re analysis" --json --model gpt-5.5 -`
	if got != want {
		t.Fatalf("quoted: got %q, want %q", got, want)
	}

	// Resume keeps the subcommand prefix before --cd.
	resumeArgs := []string{"exec", "resume", "--json", "--model", "gpt-5.5", "thread-one", "-"}
	got = buildRemoteCommandLine(`C:\Users\FLCL\re-analysis`, resumeArgs)
	want = `codex exec resume --cd C:\Users\FLCL\re-analysis --json --model gpt-5.5 thread-one -`
	if got != want {
		t.Fatalf("resume: got %q, want %q", got, want)
	}
}

// TestRemoteSSHProcess acts as a fake remote `ssh` binary. It logs its argv
// and stdin prompt to the file named by TGCC_REMOTE_SSH_LOG, then emits a
// minimal successful codex JSONL stream.
func TestRemoteSSHProcess(t *testing.T) {
	if os.Getenv("TGCC_REMOTE_SSH_HELPER") != "1" {
		return
	}
	prompt, _ := io.ReadAll(os.Stdin)
	args := os.Args
	for index, arg := range args {
		if arg == "--" {
			args = args[index+1:]
			break
		}
	}
	invocation := codexInvocation{Args: args, Prompt: string(prompt)}
	if logPath := os.Getenv("TGCC_REMOTE_SSH_LOG"); logPath != "" {
		if log, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
			_ = json.NewEncoder(log).Encode(invocation)
			_ = log.Close()
		}
	}
	fmt.Println(`{"type":"thread.started","thread_id":"remote-thread"}`)
	fmt.Println(`{"type":"turn.started"}`)
	fmt.Println(`{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"remote-ok"}}`)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	os.Exit(0)
}

func TestCodexRemoteExecution(t *testing.T) {
	st := testutil.Store(t, 10)
	topic, err := st.InsertTopic(-1, 1, "codex-remote")
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
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestRemoteSSHProcess$ -- \"$@\"\n"
	if err := os.WriteFile(sshBin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(t.TempDir(), "ssh-calls.jsonl")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := NewManager(st, nil, logger, nil, nil, "", "", "", nil, nil)
	capture := &codexCapture{}
	manager.codex = newCodexRunner(st, "codex", map[string]string{
		"TGCC_REMOTE_SSH_HELPER": "1",
		"TGCC_REMOTE_SSH_LOG":    logPath,
	}, capture, capture, logger)
	manager.codex.sshBin = sshBin
	manager.codex.retryDelay = time.Millisecond
	t.Cleanup(manager.Shutdown)

	sess, err := manager.Spawn(context.Background(), topic.ID, `C:\Users\FLCL\re-analysis`, -1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), sess.ID, "analyze this binary"); err != nil {
		t.Fatal(err)
	}
	sess = waitCodex(t, manager, sess.ID)
	if sess.Status != "idle" || sess.CodexThreadID != "remote-thread" {
		t.Fatalf("bad session: %+v", sess)
	}
	if !strings.Contains(capture.texts(), "remote-ok") {
		t.Fatalf("remote agent message not relayed: %s", capture.texts())
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var invocation codexInvocation
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &invocation); err != nil {
		t.Fatalf("decode ssh invocation: %v (data=%s)", err, data)
	}
	joined := strings.Join(invocation.Args, " ")
	if !strings.Contains(joined, "flcl@10.1.1.104") {
		t.Fatalf("missing ssh host: %s", joined)
	}
	if !strings.Contains(joined, `--cd C:\Users\FLCL\re-analysis`) {
		t.Fatalf("missing --cd remote workspace: %s", joined)
	}
	if invocation.Prompt != "analyze this binary" {
		t.Fatalf("prompt lost over stdin: %q", invocation.Prompt)
	}
}
