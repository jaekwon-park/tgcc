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
	"sync"
	"testing"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/config"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/testutil"
)

type codexCapture struct {
	mu       sync.Mutex
	messages []bot.OutgoingMsg
	pings    int
	clears   int
}

func (capture *codexCapture) Enqueue(message bot.OutgoingMsg) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.messages = append(capture.messages, message)
}

func (capture *codexCapture) Ping(int64, int64) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.pings++
}

func (capture *codexCapture) Heartbeat(int64, int64) {}

func (capture *codexCapture) Clear(int64, int64) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.clears++
}

func (capture *codexCapture) texts() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	var texts []string
	for _, message := range capture.messages {
		texts = append(texts, message.Text)
	}
	return strings.Join(texts, "\n")
}

func codexFixture(t *testing.T) (*Manager, *store.Session, *codexCapture) {
	t.Helper()
	st := testutil.Store(t, 9)
	topic, err := st.InsertTopic(-1, 1, "codex")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := st.UpdateTopicWorkspace(topic.ID, workspace); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncTopicCLI(topic.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "fake-codex")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestCodexProcess$ -- \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	manager := NewManager(st, nil, logger, nil, nil, "", "", "", nil, map[string]string{"TGCC_CODEX_HELPER": "1"})
	capture := &codexCapture{}
	manager.codex = newCodexRunner(st, bin, manager.spawnEnv, capture, capture, logger)
	manager.codex.retryDelay = time.Millisecond
	t.Cleanup(manager.Shutdown)
	sess, err := manager.Spawn(context.Background(), topic.ID, workspace, -1, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	return manager, sess, capture
}

type codexInvocation struct {
	Args   []string
	Prompt string
}

func TestCodexProcess(t *testing.T) {
	if os.Getenv("TGCC_CODEX_HELPER") != "1" {
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
	log, _ := os.OpenFile("calls.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	_ = json.NewEncoder(log).Encode(codexInvocation{Args: args, Prompt: string(prompt)})
	_ = log.Close()
	if string(prompt) == "writer" {
		data, _ := os.ReadFile("attempts")
		attempts, _ := strconv.Atoi(string(data))
		_ = os.WriteFile("attempts", []byte(strconv.Itoa(attempts+1)), 0600)
		if attempts < 2 {
			fmt.Fprintln(os.Stderr, "already has an active writer")
			os.Exit(1)
		}
	}
	if string(prompt) == "empty" {
		os.Exit(0)
	}
	if string(prompt) == "malformed" {
		fmt.Println("not JSON")
		os.Exit(0)
	}
	fmt.Println(`{"type":"thread.started","thread_id":"thread-one"}`)
	fmt.Println(`{"type":"turn.started"}`)
	if string(prompt) == "block" {
		_ = os.WriteFile("started", []byte("ready"), 0600)
		for {
			if _, err := os.Stat("release"); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if string(prompt) == "fail" {
		fmt.Println(`{"type":"turn.failed","error":{"message":"test failure"}}`)
		os.Exit(1)
	}
	text := string(prompt)
	if text == "long" {
		text = strings.Repeat("가😀", 5000)
	}
	event := map[string]any{"type": "item.completed", "item": map[string]string{"id": "message-1", "type": "agent_message", "text": text}}
	_ = json.NewEncoder(os.Stdout).Encode(event)
	_ = json.NewEncoder(os.Stdout).Encode(event)
	fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
	os.Exit(0)
}

func waitFor(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func waitCodex(t *testing.T, manager *Manager, sessionID string) *store.Session {
	t.Helper()
	waitFor(t, func() bool {
		manager.codex.mu.Lock()
		defer manager.codex.mu.Unlock()
		return manager.codex.workers[sessionID] == nil
	})
	sess, err := manager.store.SessionByID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func calls(t *testing.T, workspace string) []codexInvocation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workspace, "calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var result []codexInvocation
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var call codexInvocation
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		result = append(result, call)
	}
	return result
}

func TestCodexSerialResumeAndModel(t *testing.T) {
	manager, sess, capture := codexFixture(t)
	if err := manager.ForwardMessage(context.Background(), sess.ID, "block"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(sess.WorkspacePath, "started")); return err == nil })
	if err := manager.store.UpdateTopicModel(sess.TopicID, "gpt-5.5-test"); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardImages(context.Background(), sess.ID, "-second\nquoted ' prompt", []string{"/tmp/image with spaces.png"}); err != nil {
		t.Fatal(err)
	}
	if len(calls(t, sess.WorkspacePath)) != 1 {
		t.Fatal("second turn ran concurrently")
	}
	if err := os.WriteFile(filepath.Join(sess.WorkspacePath, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	sess = waitCodex(t, manager, sess.ID)
	if sess.CodexThreadID != "thread-one" || sess.ClaudeSessionID != "" || sess.TranscriptPath != "" || sess.Status != "idle" || sess.TurnCount != 2 || sess.PID != 0 {
		t.Fatalf("bad session: %+v", sess)
	}
	invocations := calls(t, sess.WorkspacePath)
	if len(invocations) != 2 || invocations[0].Args[1] != "--json" || invocations[1].Args[1] != "resume" {
		t.Fatalf("bad invocations: %+v", invocations)
	}
	if !strings.Contains(strings.Join(invocations[0].Args, " "), "--model gpt-5.5") || !strings.Contains(strings.Join(invocations[1].Args, " "), "--model gpt-5.5-test") {
		t.Fatalf("model selection: %+v", invocations)
	}
	if invocations[1].Prompt != "-second\nquoted ' prompt" || !strings.Contains(strings.Join(invocations[1].Args, " "), "--image /tmp/image with spaces.png thread-one -") {
		t.Fatalf("prompt/image lost: %+v", invocations[1])
	}
	if strings.Count(capture.texts(), "-second") != 1 {
		t.Fatal("duplicate agent message relayed")
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.pings != 1 || capture.clears != 1 {
		t.Fatalf("progress leak: %d/%d", capture.pings, capture.clears)
	}
}

func TestCodexOtherTopicsAndCancellation(t *testing.T) {
	manager, sess, capture := codexFixture(t)
	if err := manager.ForwardMessage(context.Background(), sess.ID, "block"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(sess.WorkspacePath, "started")); return err == nil })
	for index := 0; index < codexQueueLimit; index++ {
		if err := manager.ForwardMessage(context.Background(), sess.ID, "queued"); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.ForwardMessage(context.Background(), sess.ID, "overflow"); err == nil {
		t.Fatal("unbounded queue")
	}
	topic, err := manager.store.InsertTopic(-1, 2, "second")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.store.SyncTopicCLI(topic.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	other, err := manager.Spawn(context.Background(), topic.ID, t.TempDir(), -1, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), other.ID, "independent"); err != nil {
		t.Fatal(err)
	}
	if waitCodex(t, manager, other.ID).Status != "idle" {
		t.Fatal("other topic blocked")
	}
	if err := manager.Kill(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if waitCodex(t, manager, sess.ID).Status != "stopped" || len(calls(t, sess.WorkspacePath)) != 1 {
		t.Fatal("cancellation did not discard queue")
	}
	if !strings.Contains(capture.texts(), "16개") {
		t.Fatal("pending cancellation not reported")
	}
	if _, err := manager.Resume(context.Background(), sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), sess.ID, "resumed"); err != nil {
		t.Fatal(err)
	}
	if waitCodex(t, manager, sess.ID).Status != "idle" {
		t.Fatal("resume failed")
	}
}

func TestCodexErrorsRetryAndLongOutput(t *testing.T) {
	for _, prompt := range []string{"writer", "empty", "malformed", "fail", "long"} {
		t.Run(prompt, func(t *testing.T) {
			manager, sess, capture := codexFixture(t)
			if err := manager.ForwardMessage(context.Background(), sess.ID, prompt); err != nil {
				t.Fatal(err)
			}
			sess = waitCodex(t, manager, sess.ID)
			expected := "idle"
			if prompt == "empty" || prompt == "malformed" || prompt == "fail" {
				expected = "failed"
			}
			if sess.Status != expected {
				t.Fatalf("status %s: %s", sess.Status, capture.texts())
			}
			if prompt == "writer" && len(calls(t, sess.WorkspacePath)) != 3 {
				t.Fatal("writer retries missing")
			}
			if prompt == "long" && strings.Count(capture.texts(), "😀") != 5000 {
				t.Fatal("long output lost or duplicated")
			}
		})
	}
}

func TestCodexShutdownRefreshAndReconcile(t *testing.T) {
	manager, sess, _ := codexFixture(t)
	if err := manager.ForwardMessage(context.Background(), sess.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	waitCodex(t, manager, sess.ID)
	fresh, err := manager.FreshRestart(context.Background(), sess.ID, "ignored", -1, 1)
	if err != nil || fresh == nil || fresh.CodexThreadID != "" || fresh.ID == sess.ID {
		t.Fatalf("refresh: %+v %v", fresh, err)
	}
	old, _ := manager.store.SessionByID(sess.ID)
	if old.ArchivedAt == 0 {
		t.Fatal("old conversation not archived")
	}
	if err := manager.store.UpdateSessionStatus(fresh.ID, "active"); err != nil {
		t.Fatal(err)
	}
	reconciler := NewReconciler(manager.store, nil, nil, manager.logger)
	if err := reconciler.Run(context.Background(), "unused"); err != nil {
		t.Fatal(err)
	}
	recovered, _ := manager.store.SessionByID(fresh.ID)
	if recovered.Status != "failed" {
		t.Fatal("interrupted turn not detected")
	}
	supervisor := NewSupervisor(manager.store, manager, 0, config.DefaultContextConfig(), nil, nil, manager.logger)
	if err := manager.store.UpdateSessionStatus(fresh.ID, "crashed"); err != nil {
		t.Fatal(err)
	}
	supervisor.recover(context.Background())
	recovered, _ = manager.store.SessionByID(fresh.ID)
	if recovered.Status != "crashed" {
		t.Fatal("supervisor tried Claude recovery")
	}
	if _, err := manager.Resume(context.Background(), fresh.ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), fresh.ID, "block"); err != nil {
		t.Fatal(err)
	}
	manager.Shutdown()
	if waitCodex(t, manager, fresh.ID).Status != "stopped" {
		t.Fatal("shutdown left active worker")
	}
	if err := manager.ForwardMessage(context.Background(), fresh.ID, "late"); err == nil {
		t.Fatal("accepted request after shutdown")
	}
}
