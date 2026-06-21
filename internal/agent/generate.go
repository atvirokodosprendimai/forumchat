package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// GenerateFlush persists one progress snapshot of a streaming generation. It is
// called every FlushInterval while tokens arrive and at each transition /
// terminal state, with the accumulated body (markdown + rendered html), the
// tool trace so far, the status (StatusGenerating | StatusDone | StatusError |
// StatusInterrupted) and an error string. The caller owns persistence +
// broadcast — Generate owns the agentic loop + markdown rendering.
type GenerateFlush func(bodyMD, bodyHTML string, trace []ToolResult, status, errStr string)

// Generate drives the streaming agentic loop for agent a over msgs (the caller
// has already prepended the system prompt and dropped images for non-vision
// agents). With tools != nil it loops model → tool calls → execute → results →
// model, capped at MaxToolIterations, until the model answers with content.
// Progress is reported through flush. This is the shared core behind both the
// agent pane (agent.Runner) and the forum-thread bots (chatagents.ThreadRunner).
func Generate(ctx context.Context, prov Provider, a Agent, msgs []ChatMessage, tools ToolSet, log *slog.Logger, flush GenerateFlush) {
	var toolDefs []ToolDef
	if tools != nil {
		toolDefs = tools.Defs()
	}

	var (
		mu        sync.Mutex
		buf       strings.Builder
		dirty     bool
		toolTrace []ToolResult
	)
	appendDelta := func(s string) error {
		mu.Lock()
		buf.WriteString(s)
		dirty = true
		mu.Unlock()
		return ctx.Err()
	}
	// persist renders the buffer and hands it to flush. force=true writes even
	// when no new tokens arrived (terminal flush, or to surface fresh tool chips).
	persist := func(status, errStr string, force bool) {
		mu.Lock()
		text := buf.String()
		had := dirty
		dirty = false
		trace := append([]ToolResult(nil), toolTrace...)
		mu.Unlock()
		if !had && !force {
			return
		}
		html, herr := render.RenderMarkdown(text)
		if herr != nil {
			html = ""
		}
		flush(text, html, trace, status, errStr)
	}

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	// streamTurn runs one provider call, flushing on the ticker while it streams.
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
			persist(StatusInterrupted, "", true)
			return
		case err != nil:
			if log != nil {
				log.Warn("agent: generation failed", "model", a.Model, "err", err)
			}
			persist(StatusError, err.Error(), true)
			return
		}

		// No tool calls → the assistant content is the final answer.
		if res == nil || len(res.ToolCalls) == 0 {
			persist(StatusDone, "", true)
			return
		}
		// Iteration cap with the model still wanting tools: stop, keep the partial.
		if iter >= MaxToolIterations {
			if log != nil {
				log.Warn("agent: tool-call iteration cap reached", "model", a.Model)
			}
			persist(StatusDone, "", true)
			return
		}

		// Execute the requested tools; append the assistant tool-call turn and one
		// tool-result turn per call so the next model turn sees the results.
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

// EncodeToolCalls / DecodeToolCalls are the exported JSON codec for a tool
// trace, so callers outside this package (chatagents) can persist + read it.
func EncodeToolCalls(t []ToolResult) string { return encodeToolCalls(t) }
func DecodeToolCalls(s string) []ToolResult { return decodeToolCalls(s) }

// BuildSystemHistory prepends agent a's system prompt to msgs (with a small
// preamble) and drops images for a non-vision agent, returning the message
// slice ready for Generate. Shared so the pane and forum bots build identical
// histories. preamble is an optional surface-specific note (e.g. "answering in
// a forum thread"); empty uses just the system prompt.
func BuildSystemHistory(a Agent, preamble string, msgs []ChatMessage) []ChatMessage {
	if !a.Vision {
		msgs = stripImages(msgs)
	}
	sp := strings.TrimSpace(a.SystemPrompt)
	var system string
	switch {
	case preamble != "" && sp != "":
		system = preamble + "\n\n" + sp
	case preamble != "":
		system = preamble
	default:
		system = sp
	}
	if system != "" {
		msgs = append([]ChatMessage{{Role: RoleSystem, Content: system}}, msgs...)
	}
	return msgs
}
