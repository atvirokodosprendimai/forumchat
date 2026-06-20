package agent_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// env opens a migrated temp DB, bootstraps a community, seeds a user, and
// returns the repo + service + community id + user id.
func env(t *testing.T) (*agent.Repo, *agent.Service, string, string) {
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	u := auth.User{ID: uuid.NewString(), Email: "a@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(db).CreateUser(ctx, u); err != nil {
		t.Fatalf("user: %v", err)
	}
	repo := agent.NewRepo(db)
	return repo, agent.NewService(repo), c.ID, u.ID
}

func TestConfigRoundTrip(t *testing.T) {
	t.Parallel()
	repo, _, cid, uid := env(t)
	ctx := context.Background()

	// No row yet → disabled defaults.
	got, err := repo.GetConfig(ctx, cid)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Enabled || got.Provider != agent.ProviderOllama {
		t.Fatalf("want disabled ollama default, got %+v", got)
	}

	want := agent.Config{
		CommunityID: cid, Provider: "ollama", BaseURL: "http://h:1", Model: "m",
		SystemPrompt: "be nice", Enabled: true, UpdatedBy: uid,
	}
	if err := repo.SaveConfig(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = repo.GetConfig(ctx, cid)
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if !got.Enabled || got.Model != "m" || got.SystemPrompt != "be nice" || got.BaseURL != "http://h:1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestSendBuildsHistoryAndTitle(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()

	th, err := svc.CreateThread(ctx, cid, uid, agent.VisibilityPrivate, "m")
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	asstID, history, err := svc.Send(ctx, th, uid, "What is Go?")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	// History fed to the model = just the user turn (placeholder excluded).
	if len(history) != 1 || history[0].Role != agent.RoleUser || history[0].Content != "What is Go?" {
		t.Fatalf("history = %+v", history)
	}
	// Two rows persisted: user + generating placeholder.
	msgs, _ := repo.Messages(ctx, th.ID)
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs, got %d", len(msgs))
	}
	if msgs[1].ID != asstID || msgs[1].Status != agent.StatusGenerating || msgs[1].Role != agent.RoleAssistant {
		t.Fatalf("placeholder wrong: %+v", msgs[1])
	}
	// Title auto-derived from the first prompt.
	fresh, _ := repo.ThreadByID(ctx, th.ID)
	if fresh.Title == "" || fresh.Title == "New chat" {
		t.Fatalf("title not auto-set: %q", fresh.Title)
	}
}

func TestListThreadsVisibility(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()

	// second user
	other := auth.User{ID: uuid.NewString(), Email: "b@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(repo.DB).CreateUser(ctx, other); err != nil {
		t.Fatalf("user2: %v", err)
	}

	if _, err := svc.CreateThread(ctx, cid, uid, agent.VisibilityPrivate, ""); err != nil {
		t.Fatalf("priv: %v", err)
	}
	if _, err := svc.CreateThread(ctx, cid, uid, agent.VisibilityShared, ""); err != nil {
		t.Fatalf("shared: %v", err)
	}

	mine, _ := repo.ListThreads(ctx, cid, uid)
	if len(mine) != 2 {
		t.Fatalf("owner should see both, got %d", len(mine))
	}
	theirs, _ := repo.ListThreads(ctx, cid, other.ID)
	if len(theirs) != 1 || theirs[0].Visibility != agent.VisibilityShared {
		t.Fatalf("other should see only the shared thread, got %+v", theirs)
	}
}

func TestMarkGeneratingInterrupted(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()

	th, _ := svc.CreateThread(ctx, cid, uid, agent.VisibilityPrivate, "")
	asstID, _, err := svc.Send(ctx, th, uid, "hi")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	n, err := repo.MarkGeneratingInterrupted(ctx)
	if err != nil {
		t.Fatalf("heal: %v", err)
	}
	if n != 1 {
		t.Fatalf("want 1 healed, got %d", n)
	}
	m, _ := repo.MessageByID(ctx, asstID)
	if m.Status != agent.StatusInterrupted {
		t.Fatalf("want interrupted, got %q", m.Status)
	}
}

// stubOllama streams the given chunks as Ollama-style NDJSON then a done line.
func stubOllama(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			io.WriteString(w, `{"message":{"role":"assistant","content":`+jsonString(c)+`},"done":false}`+"\n")
			if fl != nil {
				fl.Flush()
			}
		}
		io.WriteString(w, `{"message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonString(s string) string {
	// minimal JSON string quoting for the test fixtures (no special chars used)
	return `"` + s + `"`
}

func TestOllamaStream(t *testing.T) {
	t.Parallel()
	srv := stubOllama(t, "Hel", "lo ", "Go")
	var got string
	err := agent.NewOllama(srv.URL).Stream(context.Background(), "m", []agent.ChatMessage{{Role: "user", Content: "hi"}}, func(d string) error {
		got += d
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got != "Hello Go" {
		t.Fatalf("want %q, got %q", "Hello Go", got)
	}
}

func TestRunnerEndToEnd(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	srv := stubOllama(t, "Go is ", "great.")

	th, _ := svc.CreateThread(ctx, cid, uid, agent.VisibilityPrivate, "m")
	asstID, history, err := svc.Send(ctx, th, uid, "describe Go")
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	runner := agent.NewRunner(repo, agent.NewBus(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cfg := agent.Config{CommunityID: cid, Provider: "ollama", BaseURL: srv.URL, Model: "m", Enabled: true}
	if err := runner.Start(cid, th.ID, asstID, cfg, history); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for the runner to finish (it flushes + sets status=done).
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, _ := repo.MessageByID(ctx, asstID)
		if m.Status == agent.StatusDone {
			if m.BodyMD != "Go is great." {
				t.Fatalf("want streamed body, got %q", m.BodyMD)
			}
			if m.BodyHTML == "" {
				t.Fatalf("body html not rendered")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner did not finish; last status %q", m.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runner.IsRunning(th.ID) {
		t.Fatalf("runner should have cleared the active entry")
	}
}
