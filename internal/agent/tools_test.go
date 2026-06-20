package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/agent/mcpx"
)

// seedChat inserts a user chat message so the search_fts trigger indexes it.
func seedChat(t *testing.T, repo *agent.Repo, cid, uid, body string) {
	t.Helper()
	_, err := repo.DB.ExecContext(context.Background(),
		`INSERT INTO chat_messages(id, community_id, author_id, kind, body_md, body_html, created_at)
		 VALUES(?,?,?,?,?,?,?)`,
		"cm-"+body[:3], cid, uid, "user", body, "<p>"+body+"</p>", 1)
	if err != nil {
		t.Fatalf("seed chat: %v", err)
	}
}

func TestToolsEnabledPersists(t *testing.T) {
	t.Parallel()
	repo, svc, cid, _ := env(t)
	ctx := context.Background()
	a := mkAgent(t, svc, cid, func(a *agent.Agent) { a.ToolsEnabled = true })
	got, err := repo.AgentByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if !got.ToolsEnabled {
		t.Fatal("tools_enabled did not round-trip through the DB")
	}
	// And it can be turned back off via update.
	a.ToolsEnabled = false
	if _, err := svc.SaveAgent(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, _ := repo.AgentByID(ctx, a.ID); got.ToolsEnabled {
		t.Fatal("tools_enabled should be false after update")
	}
}

func TestSearchContentFTS(t *testing.T) {
	t.Parallel()
	repo, _, cid, uid := env(t)
	ctx := context.Background()
	seedChat(t, repo, cid, uid, "datastar hypermedia is great for realtime UI")
	seedChat(t, repo, cid, uid, "forum threads keep long discussions tidy")

	hits, err := repo.SearchContent(ctx, cid, "datastar", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Kind != "chat" || !strings.Contains(hits[0].Snippet, "datastar") {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	// Scoped to community: a different community sees nothing.
	if other, _ := repo.SearchContent(ctx, "nope", "datastar", 10); len(other) != 0 {
		t.Fatalf("search leaked across communities: %+v", other)
	}
	// Garbage FTS syntax must not error.
	if _, err := repo.SearchContent(ctx, cid, `"((* AND`, 10); err != nil {
		t.Fatalf("fts syntax should be sanitized, got %v", err)
	}
}

func TestInternalMCPSearchTool(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	seedChat(t, repo, cid, uid, "datastar hypermedia rocks")

	mgr := mcpx.New(repo.SearchContent, nil, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a := mkAgent(t, svc, cid, func(a *agent.Agent) { a.ToolsEnabled = true })
	ts, err := mgr.Build(ctx, a)
	if err != nil || ts == nil {
		t.Fatalf("build toolset: ts=%v err=%v", ts, err)
	}
	defer ts.Close()

	defs := ts.Defs()
	if len(defs) != 1 || defs[0].Name != "search" {
		t.Fatalf("want one 'search' tool, got %+v", defs)
	}
	// Its schema must be a JSON-Schema object (forwarded verbatim to the model).
	var schema map[string]any
	if err := json.Unmarshal(defs[0].Schema, &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("bad schema: %s (err %v)", defs[0].Schema, err)
	}

	server, text, ok := ts.Call(ctx, "search", json.RawMessage(`{"query":"datastar"}`))
	if !ok || server != "internal" || !strings.Contains(text, "datastar") {
		t.Fatalf("call search: server=%q ok=%v text=%q", server, ok, text)
	}
	// Unknown tool is a soft error, not a panic.
	if _, _, ok := ts.Call(ctx, "nope", nil); ok {
		t.Fatal("unknown tool should report ok=false")
	}
}

// stubOllamaToolThenAnswer returns a tool call on the first /api/chat request and
// a final content answer on the second, exercising the agentic loop.
func stubOllamaToolThenAnswer(t *testing.T, firstReqBody *string) *httptest.Server {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnt := atomic.AddInt32(&n, 1)
		if cnt == 1 && firstReqBody != nil {
			b, _ := io.ReadAll(r.Body)
			*firstReqBody = string(b)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		if cnt == 1 {
			io.WriteString(w, `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"search","arguments":{"query":"datastar"}}}]},"done":true}`+"\n")
			return
		}
		io.WriteString(w, `{"message":{"role":"assistant","content":"Members love datastar."},"done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAgenticToolLoop(t *testing.T) {
	t.Parallel()
	repo, svc, cid, uid := env(t)
	ctx := context.Background()
	seedChat(t, repo, cid, uid, "datastar hypermedia is our stack")

	var firstReq string
	srv := stubOllamaToolThenAnswer(t, &firstReq)
	a := mkAgent(t, svc, cid, func(a *agent.Agent) { a.BaseURL = srv.URL; a.ToolsEnabled = true })

	th, _ := svc.CreateThread(ctx, cid, uid, a, agent.VisibilityPrivate)
	asstID, history, err := svc.Send(ctx, th, uid, "what do members think of datastar?", nil)
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	mgr := mcpx.New(repo.SearchContent, nil, false, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner := agent.NewRunner(repo, agent.NewBus(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runner.Tools = mgr.Build
	if err := runner.Start(cid, th.ID, asstID, a, history); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		m, _ := repo.MessageByID(ctx, asstID)
		if m.Status == agent.StatusDone {
			if m.BodyMD != "Members love datastar." {
				t.Fatalf("final body wrong: %q", m.BodyMD)
			}
			if len(m.ToolCalls) != 1 {
				t.Fatalf("want 1 tool call recorded, got %d (%+v)", len(m.ToolCalls), m.ToolCalls)
			}
			tc := m.ToolCalls[0]
			if tc.Name != "search" || tc.Server != "internal" || !tc.OK || !strings.Contains(tc.Result, "datastar") {
				t.Fatalf("tool trace wrong: %+v", tc)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner did not finish; last status %q", m.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The first model turn advertised the search tool.
	if !strings.Contains(firstReq, `"tools"`) || !strings.Contains(firstReq, `"search"`) {
		t.Fatalf("first request missing tool defs: %s", firstReq)
	}
}
