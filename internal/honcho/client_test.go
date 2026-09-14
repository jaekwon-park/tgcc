package honcho

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextFormats(t *testing.T) {
	for _, body := range []string{
		`{"context":"memory"}`,
		`{"summary":"memory","messages":[]}`,
		`{"summary":{"content":"memory"},"messages":[{"peer_id":"user","content":"recent"}]}`,
		`{"summary":null,"messages":[{"peer_id":"user","content":"memory"}]}`,
		`{"peer_representation":"memory","peer_card":["profile"]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) { _, _ = writer.Write([]byte(body)) }))
			defer server.Close()
			client := New(HonchoConfig{Enabled: true, BaseURL: server.URL, Workspace: "work"})
			memory, err := client.GetRepresentation(context.Background(), "topic")
			if err != nil || !strings.Contains(memory, "memory") {
				t.Fatalf("context=%q err=%v", memory, err)
			}
		})
	}
}

func TestCreateMessageV3(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != "POST" || request.URL.Path != "/v3/workspaces/work/sessions/topic/messages" {
			t.Errorf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || len(body.Messages) != 1 || body.Messages[0]["peer_id"] != "user" || body.Messages[0]["content"] != "hello" {
			t.Errorf("invalid body: %+v %v", body, err)
		}
		writer.WriteHeader(201)
	}))
	defer server.Close()
	client := New(HonchoConfig{Enabled: true, BaseURL: server.URL, Workspace: "work"})
	if err := client.CreateMessage(context.Background(), "topic", "user", "hello"); err != nil {
		t.Fatal(err)
	}
}

func TestCreateMessageSplitsUnicode(t *testing.T) {
	content := strings.Repeat("한😀", 13000)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Messages []map[string]string `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var combined strings.Builder
		for _, message := range body.Messages {
			if len([]rune(message["content"])) > 25000 {
				t.Error("message exceeds server limit")
			}
			combined.WriteString(message["content"])
		}
		if combined.String() != content || len(body.Messages) != 2 {
			t.Error("chunking changed content")
		}
		writer.WriteHeader(201)
	}))
	defer server.Close()
	client := New(HonchoConfig{Enabled: true, BaseURL: server.URL, Workspace: "work"})
	if err := client.CreateMessage(context.Background(), "topic", "user", content); err != nil {
		t.Fatal(err)
	}
}
