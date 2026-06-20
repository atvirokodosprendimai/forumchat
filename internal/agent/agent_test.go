package agent_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func mkAgent(t *testing.T, svc *agent.Service, cid string, mut func(*agent.Agent)) agent.Agent {
	t.Helper()
	a := agent.Agent{CommunityID: cid, Name: "Assistant", Provider: "ollama", BaseURL: "http://x", Model: "m", Enabled: true}
	if mut != nil {
		mut(&a)
	}
	saved, err := svc.SaveAgent(context.Background(), a)
	if err != nil {
		t.Fatalf("save agent: %v", err)
	}
	return saved
}

func TestSaveAgentCreateUpdateList(t *testing.T) {
	t.Parallel()
	repo, svc, cid, _ := env(t)
	ctx := context.Background()

	a := mkAgent(t, svc, cid, func(a *agent.Agent) { a.Name = "Coder"; a.SystemPrompt = "be terse"; a.Vision = true })
	if a.ID == "" {
		t.Fatal("expected id minted")
	}
	got, err := repo.AgentByID(ctx, a.ID)
	if err != nil || got.Name != "Coder" || !got.Vision || got.SystemPrompt != "be terse" {
		t.Fatalf("roundtrip mismatch: %+v err=%v", got, err)
	}

	// a second, disabled agent
	mkAgent(t, svc, cid, func(a *agent.Agent) { a.Name = "Draft"; a.Enabled = false })

	all, _ := repo.ListAgents(ctx, cid)
	if len(all) != 2 {
		t.Fatalf("want 2 agents, got %d", len(all))
	}
	enabled, _ := repo.ListEnabledAgents(ctx, cid)
	if len(enabled) != 1 || enabled[0].Name != "Coder" {
		t.Fatalf("want only Coder enabled, got %+v", enabled)
	}

	// update
	a.Name = "Coder v2"
	a.Enabled = false
	if _, err := svc.SaveAgent(ctx, a); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, _ := repo.AgentByID(ctx, a.ID); got.Name != "Coder v2" || got.Enabled {
		t.Fatalf("update not applied: %+v", got)
	}
}

func TestSaveAgentRequiresName(t *testing.T) {
	t.Parallel()
	_, svc, cid, _ := env(t)
	if _, err := svc.SaveAgent(context.Background(), agent.Agent{CommunityID: cid, Name: "  "}); err != agent.ErrNoName {
		t.Fatalf("want ErrNoName, got %v", err)
	}
}

func TestSendBuildsHistoryWithImages(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	a := mkAgent(t, svc, cid, func(a *agent.Agent) { a.Vision = true })

	th, err := svc.CreateThread(ctx, cid, uid, a, agent.VisibilityPrivate)
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}
	if th.AgentID != a.ID {
		t.Fatalf("thread not pinned to agent: %q", th.AgentID)
	}
	asstID, history, err := svc.Send(ctx, th, uid, "what is this?", []string{"BASE64IMG"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(history) != 1 || history[0].Role != agent.RoleUser || len(history[0].Images) != 1 || history[0].Images[0] != "BASE64IMG" {
		t.Fatalf("history image not carried: %+v", history)
	}
	msgs, _ := repo.Messages(ctx, th.ID)
	if len(msgs) != 2 || msgs[1].ID != asstID || msgs[1].Status != agent.StatusGenerating {
		t.Fatalf("placeholder wrong: %+v", msgs)
	}
	// image persisted on the user row and survives a reload
	if len(msgs[0].Images) != 1 || msgs[0].Images[0] != "BASE64IMG" {
		t.Fatalf("image not persisted: %+v", msgs[0].Images)
	}
}

func TestSwitchThreadAgent(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	a1 := mkAgent(t, svc, cid, func(a *agent.Agent) { a.Name = "Fast"; a.Model = "m1" })
	a2 := mkAgent(t, svc, cid, func(a *agent.Agent) { a.Name = "Smart"; a.Model = "m2" })

	th, _ := svc.CreateThread(ctx, cid, uid, a1, agent.VisibilityPrivate)
	if th.AgentID != a1.ID {
		t.Fatalf("thread should start on a1, got %q", th.AgentID)
	}
	if err := repo.SetThreadAgent(ctx, th.ID, a2.ID, a2.Model); err != nil {
		t.Fatalf("switch: %v", err)
	}
	got, _ := repo.ThreadByID(ctx, th.ID)
	if got.AgentID != a2.ID || got.Model != "m2" {
		t.Fatalf("switch not applied: agent=%q model=%q", got.AgentID, got.Model)
	}
}

func TestListThreadsVisibility(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	a := mkAgent(t, svc, cid, nil)

	other := auth.User{ID: uuid.NewString(), Email: "b@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(repo.DB).CreateUser(ctx, other); err != nil {
		t.Fatalf("user2: %v", err)
	}
	if _, err := svc.CreateThread(ctx, cid, uid, a, agent.VisibilityPrivate); err != nil {
		t.Fatalf("priv: %v", err)
	}
	if _, err := svc.CreateThread(ctx, cid, uid, a, agent.VisibilityShared); err != nil {
		t.Fatalf("shared: %v", err)
	}
	mine, _ := repo.ListThreads(ctx, cid, uid)
	if len(mine) != 2 {
		t.Fatalf("owner should see both, got %d", len(mine))
	}
	theirs, _ := repo.ListThreads(ctx, cid, other.ID)
	if len(theirs) != 1 || theirs[0].Visibility != agent.VisibilityShared {
		t.Fatalf("other should see only shared, got %+v", theirs)
	}
}

func TestMarkGeneratingInterrupted(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	a := mkAgent(t, svc, cid, nil)
	th, _ := svc.CreateThread(ctx, cid, uid, a, agent.VisibilityPrivate)
	asstID, _, err := svc.Send(ctx, th, uid, "hi", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	n, err := repo.MarkGeneratingInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("heal: n=%d err=%v", n, err)
	}
	if m, _ := repo.MessageByID(ctx, asstID); m.Status != agent.StatusInterrupted {
		t.Fatalf("want interrupted, got %q", m.Status)
	}
}

// stubOllama streams the chunks as Ollama NDJSON then a done line, and captures
// the request body so a test can assert images were forwarded.
func stubOllama(t *testing.T, captured *string, chunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if captured != nil {
			b, _ := io.ReadAll(r.Body)
			*captured = string(b)
		}
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

func TestOllamaStream(t *testing.T) {
	t.Parallel()
	srv := stubOllama(t, nil, "Hel", "lo ", "Go")
	var got string
	err := agent.NewOllama(srv.URL).Stream(context.Background(), "m",
		[]agent.ChatMessage{{Role: "user", Content: "hi"}}, func(d string) error { got += d; return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got != "Hello Go" {
		t.Fatalf("want %q, got %q", "Hello Go", got)
	}
}

func TestRunnerEndToEndWithImage(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	var reqBody string
	srv := stubOllama(t, &reqBody, "Go is ", "great.")

	a := mkAgent(t, svc, cid, func(a *agent.Agent) { a.BaseURL = srv.URL; a.Vision = true; a.SystemPrompt = "sys" })
	th, _ := svc.CreateThread(ctx, cid, uid, a, agent.VisibilityPrivate)
	asstID, history, err := svc.Send(ctx, th, uid, "describe", []string{"IMGDATA"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	runner := agent.NewRunner(repo, agent.NewBus(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := runner.Start(cid, th.ID, asstID, a, history); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		m, _ := repo.MessageByID(ctx, asstID)
		if m.Status == agent.StatusDone {
			if m.BodyMD != "Go is great." || m.BodyHTML == "" {
				t.Fatalf("body wrong: md=%q html=%q", m.BodyMD, m.BodyHTML)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner did not finish; last status %q", m.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if runner.IsRunning(th.ID) {
		t.Fatal("runner should have cleared the active entry")
	}
	// system prompt + image both reached the provider
	if !strings.Contains(reqBody, "sys") || !strings.Contains(reqBody, "IMGDATA") {
		t.Fatalf("request missing system prompt or image: %s", reqBody)
	}
}

// A non-vision agent must NOT forward image payloads (the model 400s on them) —
// e.g. after switching from a vision agent to a plain one mid-thread.
func TestRunnerStripsImagesForNonVisionAgent(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	var reqBody string
	srv := stubOllama(t, &reqBody, "ok")

	a := mkAgent(t, svc, cid, func(a *agent.Agent) { a.BaseURL = srv.URL; a.Vision = false })
	th, _ := svc.CreateThread(ctx, cid, uid, a, agent.VisibilityPrivate)
	asstID, history, err := svc.Send(ctx, th, uid, "describe", []string{"IMGDATA"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	runner := agent.NewRunner(repo, agent.NewBus(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := runner.Start(cid, th.ID, asstID, a, history); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if m, _ := repo.MessageByID(ctx, asstID); m.Status == agent.StatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(reqBody, "IMGDATA") {
		t.Fatalf("non-vision agent should not forward the image: %s", reqBody)
	}
}
