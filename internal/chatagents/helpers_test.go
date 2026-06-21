package chatagents_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubOllama serves a fixed NDJSON token stream from /api/chat.
func stubOllama(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			io.WriteString(w, `{"message":{"role":"assistant","content":"`+c+`"},"done":false}`+"\n")
			if fl != nil {
				fl.Flush()
			}
		}
		io.WriteString(w, `{"message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stubOllamaTools serves the tool protocol: turn 1 asks for the `search` tool,
// turn 2 (after the tool result is appended) returns the final answer. With
// tools present the provider posts stream:false, so each turn is one JSON line.
func stubOllamaTools(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		if n == 1 {
			io.WriteString(w, `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"search","arguments":{"q":"deploy"}}}]},"done":true}`+"\n")
			return
		}
		io.WriteString(w, `{"message":{"role":"assistant","content":"`+answer+`"},"done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stubToolSet is a one-tool ToolSet (search) for the runner tool-loop test.
type stubToolSet struct{ called *bool }

func (s stubToolSet) Defs() []agent.ToolDef {
	return []agent.ToolDef{{Name: "search", Description: "search", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (s stubToolSet) Call(ctx context.Context, name string, args json.RawMessage) (string, string, bool) {
	if s.called != nil {
		*s.called = true
	}
	return "internal", "found: deploy with make deploy", true
}
func (s stubToolSet) Close() {}
