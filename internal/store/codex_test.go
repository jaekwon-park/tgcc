package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaekwon-park/tgcc/internal/store"
	"github.com/jaekwon-park/tgcc/internal/testutil"
)

func TestCLIUpgradeAndSessionQueries(t *testing.T) {
	st := testutil.Store(t, 8)
	if _, err := st.DB.Exec("INSERT INTO topics(id, chat_id, thread_id, name, created_at) VALUES(1,-1,1,'old',1); INSERT INTO sessions(id,topic_id,tmux_session,tmux_window,workspace_path,status,last_activity_at,created_at) VALUES('old',1,'tgcc','@1','/tmp','idle',1,1)"); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0009_topic_cli.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.SessionByID("old")
	if err != nil || legacy.CLI != "claude" {
		t.Fatalf("legacy migration: %+v %v", legacy, err)
	}
	topic, err := st.TopicByID(1)
	if err != nil || topic.CLI != "claude" {
		t.Fatalf("topic migration: %+v %v", topic, err)
	}
	if err := st.ArchiveSession("old", 2); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncTopicCLI(1, "codex", "gpt-5.5"); err != nil {
		t.Fatal(err)
	}
	sess := &store.Session{ID: "codex", TopicID: 1, CLI: "codex", WorkspacePath: "/tmp", Status: "idle", CodexThreadID: "thread-test"}
	if err := st.InsertSession(sess); err != nil {
		t.Fatal(err)
	}
	for name, lookup := range map[string]func() (*store.Session, error){
		"id":        func() (*store.Session, error) { return st.SessionByID("codex") },
		"topic":     func() (*store.Session, error) { return st.SessionByTopicID(1) },
		"workspace": func() (*store.Session, error) { return st.SessionByWorkspaceAndStatus("/tmp", []string{"idle"}) },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := lookup()
			if err != nil || got == nil || got.CLI != "codex" || got.CodexThreadID != "thread-test" {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	active, err := st.ActiveSessions([]string{"idle"})
	if err != nil || len(active) != 1 || active[0].CLI != "codex" {
		t.Fatalf("active: %+v %v", active, err)
	}
	if got, err := st.SessionByWorkspaceNullClaudeID("/tmp"); err != nil || got != nil {
		t.Fatalf("Claude hook captured Codex: %+v %v", got, err)
	}
	if err := st.SyncTopicCLI(1, "", ""); err != nil {
		t.Fatal(err)
	}
	topic, _ = st.TopicByID(1)
	if topic.CLI != "claude" || topic.ClaudeModel.Valid {
		t.Fatalf("CLI removal did not reset model: %+v", topic)
	}
	if err := st.ArchiveSession("codex", 3); err != nil {
		t.Fatal(err)
	}
	archived, err := st.ArchivedSessionsByTopic(1)
	if err != nil || len(archived) != 2 || archived[0].CodexThreadID != "thread-test" {
		t.Fatalf("archive: %+v %v", archived, err)
	}
	var result string
	if err := st.DB.QueryRowContext(context.Background(), "PRAGMA integrity_check").Scan(&result); err != nil || result != "ok" {
		t.Fatalf("integrity: %s %v", result, err)
	}
	rows, err := st.DB.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
}
