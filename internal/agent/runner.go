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

	go r.run(ctx, cancel, prov, communityID, threadID, assistantMsgID, a.Model, msgs)
	return nil
}

func (r *Runner) run(ctx context.Context, cancel context.CancelFunc, prov Provider, communityID, threadID, msgID, model string, msgs []ChatMessage) {
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.active, threadID)
		r.mu.Unlock()
	}()
	r.Log.Info("agent: generation start", "thread", threadID, "model", model)

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

	// persist writes the current buffer to the DB and wakes the streams. DB
	// writes use a detached context with a short timeout so a Stop (which
	// cancels ctx) still flushes the final partial. force=true writes even
	// when no new tokens arrived (terminal flush).
	persist := func(status, errStr string, force bool) {
		mu.Lock()
		text := buf.String()
		had := dirty
		dirty = false
		mu.Unlock()
		if !had && !force {
			return
		}
		html, herr := render.RenderMarkdown(text)
		if herr != nil {
			html = ""
		}
		dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Repo.UpdateAssistantBody(dbCtx, msgID, text, html, status, errStr); err != nil {
			r.Log.Warn("agent: persist", "thread", threadID, "err", err)
		}
		dbCancel()
		r.broadcast(communityID, threadID)
	}

	done := make(chan error, 1)
	go func() { done <- prov.Stream(ctx, model, msgs, appendDelta) }()

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			persist(StatusGenerating, "", false)
		case err := <-done:
			status, errStr := StatusDone, ""
			switch {
			case ctx.Err() != nil:
				// User pressed Stop (or the process is shutting down): keep the
				// partial, mark it interrupted so the UI offers Regenerate.
				status = StatusInterrupted
			case err != nil:
				status, errStr = StatusError, err.Error()
				r.Log.Warn("agent: generation failed", "thread", threadID, "err", err)
			}
			persist(status, errStr, true)
			return
		}
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
