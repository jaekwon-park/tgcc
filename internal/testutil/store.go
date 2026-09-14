package testutil

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jaekwon-park/tgcc/internal/store"
)

func Store(t testing.TB, version int) *store.Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "state.db")+"?_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, source, _, _ := runtime.Caller(0)
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(source), "..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	for _, path := range paths {
		number, err := strconv.Atoi(strings.Split(filepath.Base(path), "_")[0])
		if err != nil {
			t.Fatal(err)
		}
		if number > version {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	if _, err := db.Exec("INSERT INTO users(user_id, role, created_at) VALUES(1, 'owner', 1); INSERT INTO chats(chat_id, registered_by, registered_at) VALUES(-1, 1, 1)"); err != nil {
		t.Fatal(err)
	}
	return &store.Store{DB: db}
}
