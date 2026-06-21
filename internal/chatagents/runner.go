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
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// FlushInterval batches streamed tokens into one chat fat-morph (same cadence
// and rationale as agent.FlushInterval).
const FlushInterval = 100 * time.Millisecond

// DefaultContextLimit is how many recent non-bot channel messages are replayed
// as the model's conversation context.
const DefaultContextLimit = 30

// Runner owns in-flight chat-agent generations. Each (channel, agent) pair has
// at most one goroutine streaming the model into a kind='bot' chat_messages
// row; open chat SSE streams are passive readers that refetch on every
// broadcast. Detached from any HTTP request — a browser refresh resumes live.
type Runner struct {
	Chat         *chat.Repo
	Bus          *chat.Bus
	NATS         *nats.Conn
	Log          *slog.Logger
	ContextLimit int

	mu     sync.Mutex
	active map[string]context.CancelFunc // "channelID:agentID" -> cancel
}

// NewRunner builds a Runner. limit<=0 uses DefaultContextLimit.
func NewRunner(repo *chat.Repo, bus *chat.Bus, nc *nats.Conn, limit int, log *slog.Logger) *Runner {
	if limit <= 0 {
		limit = DefaultContextLimit
	}
	return &Runner{Chat: repo, Bus: bus, NATS: nc, Log: log, ContextLimit: limit, active: map[string]context.CancelFunc{}}
}

// Trigger starts a generation for agent a in channelID. One generation per
// (channel, agent) at a time — a trigger while busy is dropped, not queued.
func (r *Runner) Trigger(communityID, channelID string, a agent.Agent) {
	key := channelID + ":" + a.ID
	r.mu.Lock()
	if _, busy := r.active[key]; busy {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.active[key] = cancel
	r.mu.Unlock()
	go r.run(ctx, cancel, key, communityID, channelID, a)
}

func (r *Runner) run(ctx context.Context, cancel context.CancelFunc, key, communityID, channelID string, a agent.Agent) {
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
	}()

	prov, err := agent.NewProvider(a)
	if err != nil {
		r.Log.Warn("chatagents: provider", "agent", a.ID, "err", err)
		return
	}

	msgs, err := r.buildHistory(ctx, channelID, a)
	if err != nil {
		r.Log.Warn("chatagents: build history", "channel", channelID, "err", err)
		return
	}
	if sp := strings.TrimSpace(a.SystemPrompt); sp != "" {
		preamble := "You are \"" + a.Name + "\", one participant in a shared group chat. " +
			"Reply concisely as a chat message.\n\n" + sp
		msgs = append([]agent.ChatMessage{{Role: agent.RoleSystem, Content: preamble}}, msgs...)
	}

	// Insert the placeholder bot bubble (detached ctx so a Stop still leaves a row).
	agentID := a.ID
	msgID := uuid.NewString()
	placeholder := chat.Message{
		ID:          msgID,
		CommunityID: communityID,
		ChannelID:   channelID,
		Kind:        chat.KindBot,
		BotName:     a.Name,
		BotAvatar:   a.AvatarURL,
		BotAgentID:  &agentID,
		GenStatus:   chat.GenGenerating,
		CreatedAt:   time.Now(),
	}
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	insErr := r.Chat.Insert(dbCtx, placeholder)
	dbCancel()
	if insErr != nil {
		r.Log.Warn("chatagents: insert placeholder", "channel", channelID, "err", insErr)
		return
	}
	r.broadcast(communityID, channelID)
	r.Log.Info("chatagents: generation start", "agent", a.ID, "channel", channelID, "model", a.Model)

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
		if err := r.Chat.UpdateBotBody(wCtx, msgID, text, html, status); err != nil {
			r.Log.Warn("chatagents: persist", "channel", channelID, "err", err)
		}
		wCancel()
		r.broadcast(communityID, channelID)
	}

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	// Chat agents stream without tools (no agentic loop in chat for v1).
	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, err := prov.Stream(ctx, a.Model, msgs, nil, appendDelta)
		done <- outcome{err}
	}()
	for {
		select {
		case <-ticker.C:
			persist(chat.GenGenerating, false)
		case o := <-done:
			switch {
			case ctx.Err() != nil:
				persist(chat.GenInterrupted, true)
			case o.err != nil:
				r.Log.Warn("chatagents: generation failed", "channel", channelID, "err", o.err)
				persist(chat.GenInterrupted, true)
			default:
				persist(chat.GenDone, true)
			}
			return
		}
	}
}

// buildHistory replays the channel's recent non-bot messages as conversation
// turns: this agent's own prior bot messages are assistant turns; everyone
// else's are user turns prefixed with the author's display name.
func (r *Runner) buildHistory(ctx context.Context, channelID string, a agent.Agent) ([]agent.ChatMessage, error) {
	recent, err := r.Chat.Recent(ctx, channelID, r.ContextLimit)
	if err != nil {
		return nil, err
	}
	// Recent is newest-first; replay oldest-first.
	out := make([]agent.ChatMessage, 0, len(recent))
	for i := len(recent) - 1; i >= 0; i-- {
		m := recent[i]
		if m.IsDeleted() {
			continue
		}
		body := strings.TrimSpace(m.BodyMarkdown)
		if body == "" {
			continue
		}
		switch m.Kind {
		case chat.KindBot:
			if m.BotAgentID != nil && *m.BotAgentID == a.ID {
				out = append(out, agent.ChatMessage{Role: agent.RoleAssistant, Content: body})
			}
			// other agents' bot messages are skipped (not our turns)
		case chat.KindUser:
			name := m.AuthorName
			if name == "" {
				name = "member"
			}
			out = append(out, agent.ChatMessage{Role: agent.RoleUser, Content: name + ": " + body})
		default:
			// system / webhook / thread_announce → skip
		}
	}
	return out, nil
}

// broadcast wakes same-process chat streams via the Bus and cross-process
// streams via NATS. Payload is the channel id — subscribers refetch.
func (r *Runner) broadcast(communityID, channelID string) {
	if r.Bus != nil {
		r.Bus.Broadcast(channelID)
	}
	if r.NATS != nil && r.NATS.IsConnected() {
		_ = r.NATS.Publish(natsx.ChatSubject(communityID), []byte(channelID))
	}
}
