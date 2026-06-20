package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
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
	prov, err := newProvider(a)
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

	msgs := history
	if !a.Vision {
		// A text-only model 400s on image input. The thread's history can still
		// carry images — e.g. after switching from a vision agent to a plain one
		// mid-conversation — so drop them for non-vision agents.
		msgs = stripImages(msgs)
	}
	if sp := strings.TrimSpace(a.SystemPrompt); sp != "" {
		msgs = append([]ChatMessage{{Role: RoleSystem, Content: sp}}, msgs...)
	}

	go r.run(ctx, cancel, prov, communityID, threadID, assistantMsgID, a, msgs)
	return nil
}

// run drives one generation to completion. With a tools-enabled agent it is an
// agentic loop: stream a turn; if the model asked for tools, execute them, append
// the results, and loop (up to MaxToolIterations) until the model answers with
// content. The streaming buffer + tool trace are flushed to the DB every
// FlushInterval and once at the end, so any open SSE stream fat-morphs live.
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
	var (
		tools     ToolSet
		toolDefs  []ToolDef
		toolTrace []ToolResult
	)
	if a.ToolsEnabled && r.Tools != nil {
		if ts, err := r.Tools(ctx, a); err != nil {
			r.Log.Warn("agent: build tools", "thread", threadID, "err", err)
		} else if ts != nil {
			tools = ts
			defer tools.Close()
			toolDefs = ts.Defs()
		}
	}

	var (
		mu    sync.Mutex
		buf   strings.Builder
		dirty bool
	)
	appendDelta := func(s string) error {
		mu.Lock()
		buf.WriteString(s)
		dirty = true
		mu.Unlock()
		return ctx.Err()
	}

	// persist writes the current buffer + tool trace to the DB and wakes the
	// streams. DB writes use a detached context with a short timeout so a Stop
	// (which cancels ctx) still flushes the final partial. force=true writes even
	// when no new tokens arrived (terminal flush, or to show fresh tool chips).
	persist := func(status, errStr string, force bool) {
		mu.Lock()
		text := buf.String()
		had := dirty
		dirty = false
		trace := encodeToolCalls(toolTrace)
		mu.Unlock()
		if !had && !force {
			return
		}
		html, herr := render.RenderMarkdown(text)
		if herr != nil {
			html = ""
		}
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Repo.UpdateAssistantBody(dbCtx, msgID, text, html, status, errStr, trace); err != nil {
			r.Log.Warn("agent: persist", "thread", threadID, "err", err)
		}
		dbCancel()
		r.broadcast(communityID, threadID)
	}

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	// streamTurn runs one provider call, flushing the buffer on the ticker while
	// it streams. Returns the StreamResult (tool calls, if any) or an error.
	streamTurn := func() (*StreamResult, error) {
		type outcome struct {
			res *StreamResult
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			res, err := prov.Stream(ctx, a.Model, msgs, toolDefs, appendDelta)
			done <- outcome{res, err}
		}()
		for {
			select {
			case <-ticker.C:
				persist(StatusGenerating, "", false)
			case o := <-done:
				return o.res, o.err
			}
		}
	}

	for iter := 0; ; iter++ {
		res, err := streamTurn()
		switch {
		case ctx.Err() != nil:
			// User pressed Stop (or shutdown): keep the partial as interrupted.
			persist(StatusInterrupted, "", true)
			return
		case err != nil:
			r.Log.Warn("agent: generation failed", "thread", threadID, "err", err)
			persist(StatusError, err.Error(), true)
			return
		}

		// No tool calls → the assistant content is the final answer.
		if res == nil || len(res.ToolCalls) == 0 {
			persist(StatusDone, "", true)
			return
		}

		// Hit the iteration cap with the model still wanting tools: stop and keep
		// whatever was produced so far rather than loop forever.
		if iter >= MaxToolIterations {
			r.Log.Warn("agent: tool-call iteration cap reached", "thread", threadID)
			persist(StatusDone, "", true)
			return
		}

		// Execute the requested tools, append the assistant tool-call turn and
		// one tool-result turn per call so the next model turn sees the results.
		msgs = append(msgs, ChatMessage{Role: RoleAssistant, ToolCalls: res.ToolCalls})
		for _, call := range res.ToolCalls {
			server, text, ok := "internal", "tools unavailable", false
			if tools != nil {
				server, text, ok = tools.Call(ctx, call.Name, call.Args)
			}
			mu.Lock()
			toolTrace = append(toolTrace, ToolResult{
				Server: server, Name: call.Name, Args: string(call.Args),
				Result: truncate(text, ToolResultDisplayMax), OK: ok,
			})
			dirty = true
			mu.Unlock()
			msgs = append(msgs, ChatMessage{Role: RoleTool, ToolName: call.Name, Content: text})
		}
		// Surface the tool chips immediately, before the next (possibly slow) turn.
		persist(StatusGenerating, "", true)
	}
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
