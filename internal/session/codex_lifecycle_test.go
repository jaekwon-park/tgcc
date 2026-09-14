package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/jaekwon-park/tgcc/internal/store"
)

func TestCodexQueueDrain(t *testing.T) {
	manager, sess, _ := codexFixture(t)
	manager.codex.drain = true
	if err := manager.ForwardMessage(context.Background(), sess.ID, "block"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(sess.WorkspacePath, "started")); return err == nil })
	if err := os.WriteFile(filepath.Join(sess.WorkspacePath, ".notify-queue"), []byte("notification"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), sess.ID, "[queue-drain]"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sess.WorkspacePath, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitCodex(t, manager, sess.ID)
	invocations := calls(t, sess.WorkspacePath)
	if len(invocations) != 2 || invocations[1].Prompt != "[queue-drain]" {
		t.Fatalf("queue drain lost or duplicated: %+v", invocations)
	}
}

func TestCodexCLISwitchArchivesOldSession(t *testing.T) {
	manager, sess, _ := codexFixture(t)
	if err := manager.store.SyncTopicCLI(sess.TopicID, "claude", ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.SyncSessionCLIs(context.Background()); err != nil {
		t.Fatal(err)
	}
	archived, err := manager.store.SessionByID(sess.ID)
	if err != nil || archived.ArchivedAt == 0 {
		t.Fatalf("Codex predecessor not archived: %+v %v", archived, err)
	}
	if err := manager.store.SyncTopicCLI(sess.TopicID, "codex", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	old := &store.Session{ID: "stopped-claude", TopicID: sess.TopicID, CLI: "claude", Status: "stopped", WorkspacePath: sess.WorkspacePath}
	if err := manager.store.InsertSession(old); err != nil {
		t.Fatal(err)
	}
	fresh, err := manager.Spawn(context.Background(), sess.TopicID, sess.WorkspacePath, -1, 1, "gpt-5.5")
	if err != nil || fresh.CLI != "codex" || fresh.CodexThreadID != "" {
		t.Fatalf("Claude to Codex transition: %+v %v", fresh, err)
	}
	old, _ = manager.store.SessionByID(old.ID)
	if old.ArchivedAt == 0 {
		t.Fatal("Claude predecessor not archived")
	}
}

func TestCodexOrphanCleanup(t *testing.T) {
	manager, sess, _ := codexFixture(t)
	command := exec.Command("/bin/sh", "-c", "sleep 30")
	command.Env = append(os.Environ(), "TGCC_CODEX_SESSION_ID="+sess.ID)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL); _ = command.Wait() })
	started, _, err := codexProcessIdentity(int64(command.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	sess.PID = int64(command.Process.Pid)
	sess.CodexProcessStart = "wrong"
	if err := stopOrphanCodex(sess); err == nil {
		t.Fatal("accepted mismatched process identity")
	}
	if err := syscall.Kill(command.Process.Pid, 0); err != nil {
		t.Fatal("killed process with mismatched identity")
	}
	if err := manager.store.UpdateSessionCodexProcess(sess.ID, sess.PID, started); err != nil {
		t.Fatal(err)
	}
	if err := manager.store.UpdateSessionStatus(sess.ID, "active"); err != nil {
		t.Fatal(err)
	}
	reconciler := NewReconciler(manager.store, nil, nil, manager.logger)
	if err := reconciler.Run(context.Background(), "unused"); err != nil {
		t.Fatal(err)
	}
	got, err := manager.store.SessionByID(sess.ID)
	if err != nil || got.Status != "failed" || got.PID != 0 || got.CodexProcessStart != "" {
		t.Fatalf("orphan reconciliation failed: %+v %v", got, err)
	}
}

func TestCodexEnvironmentAndParserLimits(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "codex-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	env := mergedCodexEnv(map[string]string{"PATH": directory, "TGCC_TEST": "kept"})
	resolved, err := resolveCodexBin("codex-test", env)
	if err != nil || resolved != path {
		t.Fatalf("spawn PATH not used: %s %v", resolved, err)
	}
	cancelled := false
	parser := &codexJSONWriter{seen: make(map[string]bool), cancel: func() { cancelled = true }, event: func(codexEvent) error { return nil }}
	if _, err := parser.Write([]byte(strings.Repeat("x", codexJSONLimit+1))); err == nil || !cancelled {
		t.Fatal("oversized JSONL was not cancelled")
	}
	parser = &codexJSONWriter{seen: make(map[string]bool), cancel: func() {}, event: func(codexEvent) error { return nil }}
	for _, chunk := range []string{"{\"type\":", "\"turn.completed\"}"} {
		if _, err := parser.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := parser.finish(); err != nil || !parser.completed {
		t.Fatalf("last line without newline: %v", err)
	}
}
