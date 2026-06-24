package chatagents

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
)

// DefaultContextLimit caps how many recent thread posts are replayed as the
// model's conversation context (the opening thread body is always included).
const DefaultContextLimit = 40

// ThreadRunner streams an agent reply into a forum thread. Each agent-owned
// thread has at most one generation in flight; open thread SSE streams refetch
// + re-render on every broadcast. Detached from any request — a refresh resumes
// live from the DB.
type ThreadRunner struct {
	Forum *forum.Repo
	Bus   *forum.Bus
	NATS  *nats.Conn
	Log   *slog.Logger
	// Tools builds the per-generation tool set for a tools-enabled agent
	// (internal FTS search + connected MCP servers) — wired in main.go to the
	// same mcpx.Manager.Build the agent pane uses. nil → bots run without tools.
	Tools        func(ctx context.Context, a agent.Agent) (agent.ToolSet, error)
	ContextLimit int

	// Resolve routes the bot reply onto platform compute (metered) for an
	// opted-in community, mirroring agent.Runner.Resolve. Wired in main.go
	// (SaaS); nil → the agent's own provider, unchanged.
	Resolve agent.ComputeResolver

	mu     sync.Mutex
	active map[string]context.CancelFunc // threadID -> cancel
}

// NewThreadRunner builds a ThreadRunner. limit<=0 uses DefaultContextLimit.
func NewThreadRunner(repo *forum.Repo, bus *forum.Bus, nc *nats.Conn, limit int, log *slog.Logger) *ThreadRunner {
	if limit <= 0 {
		limit = DefaultContextLimit
	}
	return &ThreadRunner{Forum: repo, Bus: bus, NATS: nc, Log: log, ContextLimit: limit, active: map[string]context.CancelFunc{}}
}

// Generate runs agent a over threadID's full history and streams a new bot
// reply post. One generation per thread at a time — a call while busy is
// dropped, not queued.
func (r *ThreadRunner) Generate(communityID, threadID string, a agent.Agent) {
	r.mu.Lock()
	if _, busy := r.active[threadID]; busy {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.active[threadID] = cancel
	r.mu.Unlock()
	go r.run(ctx, cancel, communityID, threadID, a)
}

func (r *ThreadRunner) run(ctx context.Context, cancel context.CancelFunc, communityID, threadID string, a agent.Agent) {
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.active, threadID)
		r.mu.Unlock()
	}()

	// Resolve compute — platform branch overrides a's provider/host/model and
	// returns a metered provider; a (not the input) drives the generation.
	var (
		prov agent.Provider
		err  error
	)
	if r.Resolve != nil {
		prov, a, err = r.Resolve(ctx, communityID, a)
	} else {
		prov, err = agent.NewProvider(a)
	}
	if err != nil {
		r.Log.Warn("chatagents: provider", "agent", a.ID, "err", err)
		return
	}
	history, err := r.buildHistory(ctx, threadID, a)
	if err != nil {
		r.Log.Warn("chatagents: build thread history", "thread", threadID, "err", err)
		return
	}
	preamble := "You are \"" + a.Name + "\", answering in a forum thread. Each message is a " +
		"prompt; reply concisely as the next post."
	msgs := agent.BuildSystemHistory(a, preamble, history)

	// Build the tool set for a tools-enabled agent (internal search + MCP) —
	// the same machinery as the agent pane. Non-fatal on failure.
	var tools agent.ToolSet
	if a.ToolsEnabled && r.Tools != nil {
		if ts, terr := r.Tools(ctx, a); terr != nil {
			r.Log.Warn("chatagents: build tools", "thread", threadID, "err", terr)
		} else if ts != nil {
			tools = ts
			defer tools.Close()
		}
	}

	agentID := a.ID
	postID := uuid.NewString()
	placeholder := forum.Post{
		ID:        postID,
		ThreadID:  threadID,
		AuthorID:  forum.AgentBotUserID,
		AgentID:   &agentID,
		BotName:   a.Name,
		BotAvatar: a.AvatarURL,
		GenStatus: forum.GenGenerating,
		CreatedAt: time.Now(),
	}
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	insErr := r.Forum.InsertBotPost(dbCtx, placeholder)
	if insErr == nil {
		_ = r.Forum.TouchThread(dbCtx, threadID, time.Now())
	}
	dbCancel()
	if insErr != nil {
		r.Log.Warn("chatagents: insert bot post", "thread", threadID, "err", insErr)
		return
	}
	r.broadcast(communityID, threadID)
	r.Log.Info("chatagents: thread generation start", "agent", a.ID, "thread", threadID, "model", a.Model)

	// Drive the shared agentic loop; each flush persists the post + tool trace
	// and wakes the thread streams.
	flush := func(md, html string, trace []agent.ToolResult, status, errStr string) {
		wCtx, wCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Forum.UpdateBotPostBody(wCtx, postID, md, html, agent.EncodeToolCalls(trace), forumGenStatus(status)); err != nil {
			r.Log.Warn("chatagents: persist bot post", "thread", threadID, "err", err)
		}
		wCancel()
		r.broadcast(communityID, threadID)
	}
	agent.Generate(ctx, prov, a, msgs, tools, r.Log, flush)
}

// forumGenStatus maps an agent generation status onto the post's gen_status.
func forumGenStatus(s string) string {
	switch s {
	case agent.StatusDone:
		return forum.GenDone
	case agent.StatusGenerating:
		return forum.GenGenerating
	default: // StatusError / StatusInterrupted — partial kept, no resume after restart
		return forum.GenInterrupted
	}
}

// buildHistory replays the thread as conversation turns: the opening thread
// body is the first user turn; this agent's own reply posts are assistant
// turns; every other post is a user turn prefixed with the author's name. Only
// the last ContextLimit posts are kept (the opening body always is).
func (r *ThreadRunner) buildHistory(ctx context.Context, threadID string, a agent.Agent) ([]agent.ChatMessage, error) {
	t, err := r.Forum.GetThread(ctx, threadID)
	if err != nil {
		return nil, err
	}
	posts, err := r.Forum.ListPosts(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if len(posts) > r.ContextLimit {
		posts = posts[len(posts)-r.ContextLimit:]
	}
	out := make([]agent.ChatMessage, 0, len(posts)+1)
	name := t.AuthorName
	if name == "" {
		name = "member"
	}
	out = append(out, agent.ChatMessage{Role: agent.RoleUser, Content: name + ": " + t.BodyMarkdown})
	for _, p := range posts {
		if p.IsDeleted() {
			continue
		}
		body := strings.TrimSpace(p.BodyMarkdown)
		if body == "" {
			continue
		}
		if p.IsBot() {
			if p.AgentID != nil && *p.AgentID == a.ID {
				out = append(out, agent.ChatMessage{Role: agent.RoleAssistant, Content: body})
			}
			continue
		}
		nm := p.AuthorName
		if nm == "" {
			nm = "member"
		}
		out = append(out, agent.ChatMessage{Role: agent.RoleUser, Content: nm + ": " + body})
	}
	return out, nil
}

// broadcast wakes same-process thread streams via the Bus and cross-process
// streams via NATS. Payload is just "changed" — subscribers refetch.
func (r *ThreadRunner) broadcast(communityID, threadID string) {
	if r.Bus != nil {
		r.Bus.Broadcast(threadID)
	}
	if r.NATS != nil && r.NATS.IsConnected() {
		_ = r.NATS.Publish(natsx.ForumThreadSubject(communityID, threadID), []byte("changed"))
	}
}
