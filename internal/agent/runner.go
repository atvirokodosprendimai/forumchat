package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/nats-io/nats.go"
)

// FlushInterval is the cadence at which the streaming buffer is written to the
// DB and broadcast to open SSE streams. 100ms batches many tokens into one
// fat-morph: with brotli the repeated full-conversation patch compresses ~20x,
// and the batched arrival reads as a smooth view-transition "burst" rather
// than per-token jitter.
const FlushInterval = 100 * time.Millisecond

// Runner owns in-flight generations. Each active thread has exactly one
// goroutine streaming the model into the DB; the SSE streams are passive
// readers that refetch on every broadcast. Detached from any HTTP request, so
// a browser refresh/crash never kills a generation — the DB keeps filling and
// the next stream connection resumes live.
type Runner struct {
	Repo *Repo
	Bus  *Bus
	NATS *nats.Conn
	Log  *slog.Logger

	// Tools builds the live tool set for a tools-enabled agent (internal search
	// + the community's connected MCP servers). Wired in main.go; nil means tool
	// support is unavailable, in which case the runner streams without tools even
	// for a tools-enabled agent. The returned ToolSet is closed when the
	// generation ends.
	Tools func(ctx context.Context, a Agent) (ToolSet, error)

	// Resolve routes the generation onto platform compute (metered) for an
	// opted-in community, or leaves it on the agent's own provider. Wired in
	// main.go (SaaS); nil → the agent's BYO provider, unchanged.
	Resolve ComputeResolver

	mu     sync.Mutex
	active map[string]context.CancelFunc // threadID -> cancel
}

// NewRunner builds a Runner.
func NewRunner(repo *Repo, bus *Bus, nc *nats.Conn, log *slog.Logger) *Runner {
	return &Runner{Repo: repo, Bus: bus, NATS: nc, Log: log, active: map[string]context.CancelFunc{}}
}

// IsRunning reports whether a generation is in flight for threadID.
func (r *Runner) IsRunning(threadID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.active[threadID]
	return ok
}

// Stop cancels an in-flight generation. The runner persists the partial answer
// as "interrupted" before it exits. No-op if nothing is running.
func (r *Runner) Stop(threadID string) {
	r.mu.Lock()
	cancel := r.active[threadID]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Start launches a generation for assistantMsgID in threadID using the agent's
// provider/model/system-prompt and the conversation history. Refuses (returns
// ErrGenerating) if a generation is already in flight for the thread. The
// caller has already inserted the empty assistant placeholder (status=generating).
func (r *Runner) Start(communityID, threadID, assistantMsgID string, a Agent, history []ChatMessage) error {
	// Resolve compute first — on the platform branch this overrides a's
	// provider/host/model and returns a metered provider; a (not the input) is
	// what the generation must run with, since Generate streams against a.Model.
	prov, a, err := resolveProvider(context.Background(), r.Resolve, communityID, a)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if _, busy := r.active[threadID]; busy {
		r.mu.Unlock()
		return ErrGenerating
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.active[threadID] = cancel
	r.mu.Unlock()

	// Prepend the system prompt + drop images for a non-vision agent (a text
	// model 400s on image input — the thread history can still carry them after
	// a vision→plain agent switch). Shared with the forum-thread bots.
	msgs := BuildSystemHistory(a, "", history)

	go r.run(ctx, cancel, prov, communityID, threadID, assistantMsgID, a, msgs)
	return nil
}

// run drives one generation to completion via the shared agentic loop
// (agent.Generate), persisting each progress snapshot to ai_messages and waking
// the thread's SSE streams. DB writes use a detached context with a short
// timeout so a Stop (which cancels ctx) still flushes the final partial.
func (r *Runner) run(ctx context.Context, cancel context.CancelFunc, prov Provider, communityID, threadID, msgID string, a Agent, msgs []ChatMessage) {
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.active, threadID)
		r.mu.Unlock()
	}()
	r.Log.Info("agent: generation start", "thread", threadID, "model", a.Model)

	// Build the tool set for a tools-enabled agent. Failure to connect is
	// non-fatal: log and run without tools rather than failing the whole turn.
	var tools ToolSet
	if a.ToolsEnabled && r.Tools != nil {
		if ts, err := r.Tools(ctx, a); err != nil {
			r.Log.Warn("agent: build tools", "thread", threadID, "err", err)
		} else if ts != nil {
			tools = ts
			defer tools.Close()
		}
	}

	flush := func(md, html string, trace []ToolResult, status, errStr string) {
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Repo.UpdateAssistantBody(dbCtx, msgID, md, html, status, errStr, encodeToolCalls(trace)); err != nil {
			r.Log.Warn("agent: persist", "thread", threadID, "err", err)
		}
		dbCancel()
		r.broadcast(communityID, threadID)
	}
	Generate(ctx, prov, a, msgs, tools, r.Log, flush)
}

// stripImages returns a copy of the history with all image payloads removed,
// for providers/models that don't accept image input.
func stripImages(in []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, len(in))
	for i, m := range in {
		m.Images = nil
		out[i] = m
	}
	return out
}

// broadcast wakes same-process streams via the Bus and cross-process streams
// via NATS. Payload is just the thread id — subscribers refetch from the DB.
func (r *Runner) broadcast(communityID, threadID string) {
	if r.Bus != nil {
		r.Bus.Broadcast(threadID)
	}
	if r.NATS != nil && r.NATS.IsConnected() {
		_ = r.NATS.Publish(natsx.AgentThreadSubject(communityID, threadID), []byte(threadID))
	}
}
