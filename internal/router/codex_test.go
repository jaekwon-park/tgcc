package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaekwon-park/tgcc/internal/bot"
	"github.com/jaekwon-park/tgcc/internal/session"
	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/testutil"
)

type codexTransport struct {
	mu    sync.Mutex
	texts []string
}

func (transport *codexTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		return nil, err
	}
	transport.mu.Lock()
	transport.texts = append(transport.texts, payload.Text)
	transport.mu.Unlock()
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":1}}`))}, nil
}

func TestCodexCommands(t *testing.T) {
	st := testutil.Store(t, 9)
	topic, err := st.InsertTopic(-1, 1, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SyncTopicCLI(topic.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateTopicWorkspace(topic.ID, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	transport := &codexTransport{}
	original := http.DefaultTransport
	http.DefaultTransport = transport
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client := bot.NewClient("fake")
	sender := bot.NewSender(client, logger)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = sender.Start(ctx); close(done) }()
	manager := session.NewManager(st, nil, logger, sender, nil, "", "", "", nil, nil)
	t.Cleanup(func() {
		manager.Shutdown()
		cancel()
		<-done
		http.DefaultTransport = original
	})
	router := NewRouter(st, logger, sender, nil, nil, manager, nil, nil, nil, "", "", client, "", nil)
	run := func(command string) {
		t.Helper()
		update := bot.Update{Message: &bot.Message{Chat: &bot.Chat{ID: -1}, MessageThreadID: 1, Text: command}}
		if err := router.Route(ctx, update, &store.User{UserID: 1, Role: "owner"}); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
	run("/new")
	first, _ := st.SessionByTopicID(topic.ID)
	run("/model gpt-5.5")
	afterModel, _ := st.SessionByTopicID(topic.ID)
	if first.ID != afterModel.ID {
		t.Fatal("model change restarted Codex")
	}
	for _, command := range []string{"/compact", "/ctxstatus", "/squash 2", "/ctxconfig", "/status", "/list", "/stop", "/resume", "/kill", "/resume", "/refresh", "/restart", "/list-archived"} {
		run(command)
	}
	afterRefresh, _ := st.SessionByTopicID(topic.ID)
	if afterRefresh.ID == first.ID || afterRefresh.CLI != "codex" {
		t.Fatal("refresh did not create Codex session")
	}
	flushCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := sender.Flush(flushCtx); err != nil {
		t.Fatal(err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	text := strings.Join(transport.texts, "\n")
	if strings.Count(text, "아직 지원하지 않습니다") != 4 || !strings.Contains(text, "CLI: codex") || !strings.Contains(text, "다음 요청부터 적용") {
		t.Fatalf("missing command feedback: %s", text)
	}
}
