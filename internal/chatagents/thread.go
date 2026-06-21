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
	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// FlushInterval batches streamed tokens into one forum-thread re-render (same
// cadence + rationale as agent.FlushInterval).
const FlushInterval = 100 * time.Millisecond

// DefaultContextLimit caps how many recent thread posts are replayed as the
// model's conversation context (the opening thread body is always included).
const DefaultContextLimit = 40

// ThreadRunner streams an agent reply into a forum thread. Each agent-owned
// thread has at most one generation in flight; open thread SSE streams refetch
// + re-render on every broadcast. Detached from any request — a refresh resumes
// live from the DB.
type ThreadRunner struct {
	Forum        *forum.Repo
	Bus          *forum.Bus
	NATS         *nats.Conn
	Log          *slog.Logger
	ContextLimit int

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

	prov, err := agent.NewProvider(a)
	if err != nil {
		r.Log.Warn("chatagents: provider", "agent", a.ID, "err", err)
		return
	}
	msgs, err := r.buildHistory(ctx, threadID, a)
	if err != nil {
		r.Log.Warn("chatagents: build thread history", "thread", threadID, "err", err)
		return
	}
	if sp := strings.TrimSpace(a.SystemPrompt); sp != "" {
		preamble := "You are \"" + a.Name + "\", answering in a forum thread. Each message is a " +
			"prompt; reply concisely as the next post.\n\n" + sp
		msgs = append([]agent.ChatMessage{{Role: agent.RoleSystem, Content: preamble}}, msgs...)
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

	var (
		bufMu sync.Mutex
		buf   strings.Builder
		dirty bool
	)
	appendDelta := func(s string) error {
		bufMu.Lock()
		buf.WriteString(s)
		dirty = true
		bufMu.Unlock()
		return ctx.Err()
	}
	persist := func(status string, force bool) {
		bufMu.Lock()
		text := buf.String()
		had := dirty
		dirty = false
		bufMu.Unlock()
		if !had && !force {
			return
		}
		html, herr := render.RenderMarkdown(text)
		if herr != nil {
			html = ""
		}
		wCtx, wCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Forum.UpdateBotPostBody(wCtx, postID, text, html, status); err != nil {
			r.Log.Warn("chatagents: persist bot post", "thread", threadID, "err", err)
		}
		wCancel()
		r.broadcast(communityID, threadID)
	}

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, err := prov.Stream(ctx, a.Model, msgs, nil, appendDelta)
		done <- outcome{err}
	}()
	for {
		select {
		case <-ticker.C:
			persist(forum.GenGenerating, false)
		case o := <-done:
			switch {
			case ctx.Err() != nil:
				persist(forum.GenInterrupted, true)
			case o.err != nil:
				r.Log.Warn("chatagents: thread generation failed", "thread", threadID, "err", o.err)
				persist(forum.GenInterrupted, true)
			default:
				persist(forum.GenDone, true)
			}
			return
		}
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
