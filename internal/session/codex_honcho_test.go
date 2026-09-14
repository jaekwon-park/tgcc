package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jaekwon-park/tgcc/internal/honcho"
)

func writeCodexTranscript(t *testing.T, path, thread string) {
	t.Helper()
	entries := []any{
		map[string]any{"type": "session_meta", "payload": map[string]string{"id": thread}},
		map[string]any{"type": "event_msg", "payload": map[string]string{"type": "user_message", "message": "duplicate"}},
	}
	for _, message := range []struct{ role, text string }{
		{"developer", "private instructions"},
		{"user", "# AGENTS.md instructions for workspace"},
		{"user", "[Honcho Memory]\nold injected memory\n\n[Current User Message]\nremember CODE-914"},
		{"assistant", "I remember CODE-914"},
	} {
		entries = append(entries, map[string]any{"type": "response_item", "payload": map[string]any{
			"type": "message", "role": message.role, "content": []map[string]string{{"type": "input_text", "text": message.text}},
		}})
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, entry := range entries {
		if err := json.NewEncoder(file).Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexHonchoLifecycle(t *testing.T) {
	for _, action := range []string{"stop", "kill", "refresh", "switch"} {
		t.Run(action, func(t *testing.T) {
			manager, sess, _ := codexFixture(t)
			home := t.TempDir()
			directory := filepath.Join(home, "sessions", "2026", "09", "14")
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			writeCodexTranscript(t, filepath.Join(directory, "rollout-test-thread-one.jsonl"), "thread-one")
			manager.spawnEnv["CODEX_HOME"] = home
			if err := manager.store.UpdateSessionCodexThread(sess.ID, "thread-one"); err != nil {
				t.Fatal(err)
			}
			var mutex sync.Mutex
			var saved string
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mutex.Lock()
				defer mutex.Unlock()
				if request.Method == http.MethodPost {
					if !strings.HasSuffix(request.URL.Path, "/messages") {
						t.Errorf("unexpected path: %s", request.URL.Path)
					}
					var body struct {
						Messages []struct {
							Content string `json:"content"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.Messages) != 1 {
						t.Errorf("invalid message body: %v", err)
						writer.WriteHeader(400)
						return
					}
					saved = body.Messages[0].Content
					writer.WriteHeader(201)
					return
				}
				reads++
				_ = json.NewEncoder(writer).Encode(map[string]any{"messages": []map[string]string{{"peer_id": "user", "content": saved}}})
			}))
			defer server.Close()
			manager.honchoClient = honcho.New(honcho.HonchoConfig{Enabled: true, BaseURL: server.URL, Workspace: "work"})
			manager.codex.loadMemory = manager.loadCodexMemory
			ctx := context.Background()
			switch action {
			case "stop":
				if err := manager.Stop(ctx, sess.ID); err != nil {
					t.Fatal(err)
				}
			case "kill":
				if err := manager.Kill(ctx, sess.ID); err != nil {
					t.Fatal(err)
				}
			case "refresh":
				fresh, err := manager.FreshRestart(ctx, sess.ID, "", -1, 1)
				if err != nil {
					t.Fatal(err)
				}
				sess = fresh
			case "switch":
				if err := manager.store.SyncTopicCLI(sess.TopicID, "claude", ""); err != nil {
					t.Fatal(err)
				}
				if err := manager.SyncSessionCLIs(ctx); err != nil {
					t.Fatal(err)
				}
			}
			mutex.Lock()
			stored := saved
			mutex.Unlock()
			if !strings.Contains(stored, "user: remember CODE-914") || !strings.Contains(stored, "assistant: I remember CODE-914") || strings.Contains(stored, "injected memory") || strings.Contains(stored, "duplicate") || strings.Contains(stored, "private instructions") {
				t.Fatalf("incorrect saved conversation: %q", stored)
			}
			if action == "switch" {
				return
			}
			if action != "refresh" {
				fresh, err := manager.Spawn(ctx, sess.TopicID, sess.WorkspacePath, -1, 1, "")
				if err != nil {
					t.Fatal(err)
				}
				sess = fresh
			}
			if err := manager.ForwardMessage(ctx, sess.ID, "recall"); err != nil {
				t.Fatal(err)
			}
			waitCodex(t, manager, sess.ID)
			if err := manager.ForwardMessage(ctx, sess.ID, "next"); err != nil {
				t.Fatal(err)
			}
			waitCodex(t, manager, sess.ID)
			invocations := calls(t, sess.WorkspacePath)
			if len(invocations) != 2 || !strings.Contains(invocations[0].Prompt, "[Honcho Memory]") || !strings.Contains(invocations[0].Prompt, "CODE-914") || invocations[1].Prompt != "next" {
				t.Fatalf("memory injection/resume failed: %+v", invocations)
			}
			mutex.Lock()
			defer mutex.Unlock()
			if reads != 1 {
				t.Fatalf("memory fetched %d times", reads)
			}
		})
	}
}

func TestCodexHonchoUnavailableAndTranscriptValidation(t *testing.T) {
	manager, sess, _ := codexFixture(t)
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	writeCodexTranscript(t, path, "thread-one")
	if _, err := readCodexConversation(path, "wrong-thread"); err == nil {
		t.Fatal("accepted mismatched thread")
	}
	if err := manager.store.UpdateSessionTranscriptPath(sess.ID, path); err != nil {
		t.Fatal(err)
	}
	if err := manager.store.UpdateSessionCodexThread(sess.ID, "thread-one"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { writer.WriteHeader(503) }))
	defer server.Close()
	manager.honchoClient = honcho.New(honcho.HonchoConfig{Enabled: true, BaseURL: server.URL, Workspace: "work"})
	manager.codex.loadMemory = manager.loadCodexMemory
	fresh, err := manager.FreshRestart(context.Background(), sess.ID, "", -1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.ForwardMessage(context.Background(), fresh.ID, "recall"); err != nil {
		t.Fatal(err)
	}
	result := waitCodex(t, manager, fresh.ID)
	if result.Status != "idle" {
		t.Fatalf("Honcho failure blocked turn: %s", result.Status)
	}
	if invocation := calls(t, sess.WorkspacePath)[0]; !strings.Contains(invocation.Prompt, "CODE-914") {
		t.Fatal("recent conversation fallback lost")
	}
}
