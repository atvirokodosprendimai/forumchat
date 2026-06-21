package chatagents_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
