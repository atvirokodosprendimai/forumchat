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
)

// DefaultChannelContext caps how many recent channel messages are replayed as
// the model's conversation context (oldest dropped first). Chat has no threads,
// so the whole tail of the channel — humans AND other bots — is the context.
const DefaultChannelContext = 30

// ChannelRunner streams an agent reply as a kind='bot' bubble directly in a chat
// channel — the in-channel counterpart to the forum ThreadRunner. One generation
// per (channel, agent) is in flight at a time; the chat SSE streams refetch +
// fat-morph on every broadcast, so the bubble grows live and a browser refresh
// resumes from the DB for free (a server restart can't — the boot sweep flips
// generating→interrupted).
type ChannelRunner struct {
	Chat *chat.Repo
	// Bus wakes same-process chat streams to re-render; NewMsgBus additionally
	// fires the "a new message landed" event (notification ping). The placeholder
	// insert pings both; per-flush growth pings only Bus (no repeat ping).
	Bus       *chat.Bus
	NewMsgBus *chat.Bus
	NATS      *nats.Conn
	Log       *slog.Logger
	// Tools builds the per-generation tool set for a tools-enabled agent — wired
	// in main.go to the same mcpx.Manager.Build the pane uses. nil → no tools.
	Tools        func(ctx context.Context, a agent.Agent) (agent.ToolSet, error)
	ContextLimit int
	// Resolve routes the reply onto platform compute (metered) for an opted-in
	// community, mirroring agent.Runner.Resolve. nil → the agent's own provider.
	Resolve agent.ComputeResolver
	// OnReply, when set, is called with the completed reply text so a bound
	// dispatcher can re-trigger OTHER agents (bot-to-bot). Wired by the
	// Dispatcher; nil disables the bot-to-bot loop. The call is the ONLY way an
	// agent message re-enters dispatch, so the /autochat gate + 15s cooldown
	// there fully bound the loop.
	OnReply func(communityID, channelID, slug, authorAgentID, body string)

	mu     sync.Mutex
	active map[string]context.CancelFunc // "channelID|agentID" -> cancel
}

// NewChannelRunner builds a ChannelRunner. limit<=0 uses DefaultChannelContext.
func NewChannelRunner(repo *chat.Repo, bus, newMsgBus *chat.Bus, nc *nats.Conn, limit int, log *slog.Logger) *ChannelRunner {
	if limit <= 0 {
		limit = DefaultChannelContext
	}
	return &ChannelRunner{
		Chat: repo, Bus: bus, NewMsgBus: newMsgBus, NATS: nc,
		ContextLimit: limit, Log: log, active: map[string]context.CancelFunc{},
	}
}

// Generate streams agent a's reply into channelID as a new kind='bot' bubble.
// One generation per (channel, agent) — a call while that pair is busy is
// dropped, not queued (prevents a single agent stacking replies). slug is
// carried only so a bot-to-bot re-dispatch can address the right community.
func (r *ChannelRunner) Generate(communityID, channelID, slug string, a agent.Agent) {
	key := channelID + "|" + a.ID
	r.mu.Lock()
	if _, busy := r.active[key]; busy {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.active[key] = cancel
	r.mu.Unlock()
	go r.run(ctx, cancel, communityID, channelID, slug, a)
}

func (r *ChannelRunner) run(ctx context.Context, cancel context.CancelFunc, communityID, channelID, slug string, a agent.Agent) {
	key := channelID + "|" + a.ID
	defer func() {
		cancel()
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
	}()

	// Resolve compute — the platform branch overrides a's provider/host/model and
	// returns a metered provider; the returned agent (not the input) drives gen.
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
		r.Log.Warn("chatagents: channel provider", "agent", a.ID, "err", err)
		return
	}

	// Build the conversation context BEFORE inserting the placeholder so the
	// bot's own empty bubble never leaks into its own prompt.
	history, err := r.buildHistory(ctx, channelID, a)
	if err != nil {
		r.Log.Warn("chatagents: build channel history", "channel", channelID, "err", err)
		return
	}
	preamble := "You are \"" + a.Name + "\", a participant in a live group chat channel. " +
		"Each line is one message; reply concisely and conversationally as the next message."
	msgs := agent.BuildSystemHistory(a, preamble, history)

	// Tool set for a tools-enabled agent (internal search + MCP), same as the
	// pane. Non-fatal on failure.
	var tools agent.ToolSet
	if a.ToolsEnabled && r.Tools != nil {
		if ts, terr := r.Tools(ctx, a); terr != nil {
			r.Log.Warn("chatagents: build channel tools", "channel", channelID, "err", terr)
		} else if ts != nil {
			tools = ts
			defer tools.Close()
		}
	}

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
		BotAsHuman:  a.ChatAsHuman, // render as a regular member when set
		GenStatus:   chat.GenGenerating,
		CreatedAt:   time.Now(),
	}
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 5*time.Second)
	insErr := r.Chat.Insert(dbCtx, placeholder)
	dbCancel()
	if insErr != nil {
		r.Log.Warn("chatagents: insert bot bubble", "channel", channelID, "err", insErr)
		return
	}
	r.broadcastNew(communityID, channelID)
	r.Log.Info("chatagents: channel generation start", "agent", a.ID, "channel", channelID, "model", a.Model)

	// Drive the shared agentic loop. Each flush persists the growing body and
	// wakes the channel streams; we keep the last body + status for the
	// bot-to-bot hand-off after the loop returns.
	var lastBody, lastStatus string
	flush := func(md, html string, _ []agent.ToolResult, status, _ string) {
		lastBody, lastStatus = md, status
		wCtx, wCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.Chat.UpdateBotBody(wCtx, msgID, md, html, chatGenStatus(status)); err != nil {
			r.Log.Warn("chatagents: persist bot bubble", "channel", channelID, "err", err)
		}
		wCancel()
		r.broadcast(communityID, channelID)
	}
	agent.Generate(ctx, prov, a, msgs, tools, r.Log, flush)

	// Bot-to-bot: hand the finished reply back to dispatch so another agent it
	// addressed can answer. Only on a clean finish with real content — an
	// interrupted/empty reply isn't worth re-triggering. The /autochat gate +
	// per-agent cooldown in the dispatcher decide whether anything actually runs.
	if r.OnReply != nil && lastStatus == agent.StatusDone {
		if body := strings.TrimSpace(lastBody); body != "" {
			r.OnReply(communityID, channelID, slug, a.ID, body)
		}
	}
}

// chatGenStatus maps an agent generation status onto the chat bubble's
// gen_status lifecycle.
func chatGenStatus(s string) string {
	switch s {
	case agent.StatusDone:
		return chat.GenDone
	case agent.StatusGenerating:
		return chat.GenGenerating
	default: // StatusError / StatusInterrupted — partial kept, no resume
		return chat.GenInterrupted
	}
}

// buildHistory replays the channel tail as conversation turns: this agent's own
// past bubbles become assistant turns; every other message (humans and OTHER
// bots) becomes a user turn prefixed with the speaker's name. Only the last
// ContextLimit non-empty, non-system messages are kept.
func (r *ChannelRunner) buildHistory(ctx context.Context, channelID string, a agent.Agent) ([]agent.ChatMessage, error) {
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
		if m.Kind == chat.KindSystem || m.Kind == chat.KindThreadAnnounce {
			continue
		}
		body := strings.TrimSpace(m.BodyMarkdown)
		if body == "" {
			continue
		}
		if m.Kind == chat.KindBot && m.BotAgentID != nil && *m.BotAgentID == a.ID {
			out = append(out, agent.ChatMessage{Role: agent.RoleAssistant, Content: body})
			continue
		}
		// Every other speaker (humans AND other bots) is untrusted input — the
		// shared constructor sanitizes the label + body so a member can't forge
		// turns or smuggle hidden instructions. See agent.UntrustedTurn.
		out = append(out, agent.UntrustedTurn(m.AuthorName, body))
	}
	return out, nil
}

// broadcastNew fans out a brand-new bubble: same-process streams (Bus) + the
// new-message event (NewMsgBus) + cross-process (NATS chat + chat-new).
func (r *ChannelRunner) broadcastNew(communityID, channelID string) {
	r.broadcast(communityID, channelID)
	if r.NewMsgBus != nil {
		r.NewMsgBus.Broadcast(channelID)
	}
	if r.NATS != nil && r.NATS.IsConnected() {
		_ = r.NATS.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
	}
}

// broadcast wakes same-process + cross-process chat streams to re-render the
// channel (no "new message" ping — used for the per-flush streaming growth).
func (r *ChannelRunner) broadcast(communityID, channelID string) {
	if r.Bus != nil {
		r.Bus.Broadcast(channelID)
	}
	if r.NATS != nil && r.NATS.IsConnected() {
		_ = r.NATS.Publish(natsx.ChatSubject(communityID), []byte(channelID))
	}
}
