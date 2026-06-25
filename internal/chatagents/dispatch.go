package chatagents

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/agentlimit"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
)

// BotReplyCooldown is the minimum wall-clock gap between two consecutive
// bot-TRIGGERED replies by the SAME agent in the SAME channel — the "15s
// between responses" throttle on bot-to-bot conversation. It bounds an
// otherwise-endless ping-pong (there is deliberately no auto-stop; an admin
// halts it with /autochat 0). It does NOT gate human-triggered replies — those
// use the per-user/community agentlimit budget instead.
const BotReplyCooldown = 15 * time.Second

// AgentSource supplies the chat-participating agents bound to a channel.
// Satisfied by *agent.Repo.
type AgentSource interface {
	AgentsForChannel(ctx context.Context, communityID, channelID string) ([]agent.Agent, error)
}

// RateGate decides whether a member may trigger an agent right now, recording
// the trigger when allowed. Satisfied by *agentlimit.Gate; nil disables
// throttling.
type RateGate interface {
	Check(ctx context.Context, communityID, userID string, isSuperAdmin bool) agentlimit.Decision
}

// PolicyFunc reports a community's channel-agent switches: bots is the /bots
// master (do bound agents answer channel messages at all) and autochat is the
// /autochat switch (may agents trigger EACH OTHER, bot-to-bot). nil Policy →
// bots on, autochat off — the defaults that preserve the pre-feature behaviour
// (and keep the forum-thread unit tests, which don't set a Policy, unchanged).
type PolicyFunc func(ctx context.Context, communityID string) (bots, autochat bool)

// DispatchResult reports whether a trigger was throttled, so the caller can
// surface a notice. The zero value means "not rate-limited".
type DispatchResult struct {
	RateLimited bool
	RetryAfter  time.Duration
}

// CreateThreadFunc opens an agent-owned forum thread for a triggered agent and
// announces it in chat, returning the thread id. Wired in main.go to
// forum.Handler.CreateAgentThread (the chat→forum bridge), so chatagents needn't
// import forum's handler.
type CreateThreadFunc func(ctx context.Context, communityID, slug, authorID, agentID, agentName, prompt string) (threadID string, err error)

// Trigger is one chat message considered for agent dispatch.
type Trigger struct {
	CommunityID string
	Slug        string
	ChannelID   string
	AuthorID    string
	AuthorName  string
	Body        string
	Kind        chat.Kind
	// AuthorAgentID is the agent that authored a Kind==KindBot trigger, so the
	// bot-to-bot pass never lets an agent answer itself. Empty for human sends.
	AuthorAgentID string
	// IsSuperAdmin exempts the trigger from agent rate limits (platform
	// super-admins are never throttled).
	IsSuperAdmin bool
}

// Dispatcher routes a freshly-sent chat message to the agents it triggers. A
// human send opens a forum thread (the formal, resumable answer) AND streams an
// in-channel bubble; a bot's own reply (when /autochat is on) re-enters here and
// streams an in-channel bubble only — keeping bot-to-bot banter in the channel,
// not spawning a thread per turn.
type Dispatcher struct {
	Agents       AgentSource
	CreateThread CreateThreadFunc
	Runner       *ThreadRunner  // forum-thread streamer (human triggers)
	Channel      *ChannelRunner // in-channel streamer (both human + bot); nil disables in-channel replies
	Gate         RateGate       // optional; nil disables human rate limiting
	Policy       PolicyFunc     // optional; nil → bots on, autochat off
	Log          *slog.Logger

	// cooldown enforces BotReplyCooldown per (channel, agent) for bot-to-bot.
	// Process-local: it's a cost throttle, not a correctness gate, so exactness
	// across processes isn't required (each process also caps one in-flight
	// generation per (channel, agent) via the runner's active map).
	cooldownMu sync.Mutex
	cooldownAt map[string]time.Time
}

// NewDispatcher builds a Dispatcher. gate may be nil to disable rate limiting.
// Channel and Policy are set by the caller after construction (main.go) — left
// nil they preserve the forum-thread-only, always-on behaviour.
func NewDispatcher(agents AgentSource, create CreateThreadFunc, runner *ThreadRunner, gate RateGate, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		Agents: agents, CreateThread: create, Runner: runner, Gate: gate, Log: log,
		cooldownAt: map[string]time.Time{},
	}
}

// allowBotReply reserves a bot-to-bot reply slot for (channelID, agentID): it
// returns true (and stamps "now") only when at least BotReplyCooldown has
// elapsed since this agent last replied in this channel. Check-and-reserve is
// atomic under the mutex so two concurrent bot messages can't both slip through.
func (d *Dispatcher) allowBotReply(channelID, agentID string) bool {
	key := channelID + "|" + agentID
	now := time.Now()
	d.cooldownMu.Lock()
	defer d.cooldownMu.Unlock()
	if last, ok := d.cooldownAt[key]; ok && now.Sub(last) < BotReplyCooldown {
		return false
	}
	d.cooldownAt[key] = now
	return true
}

// Dispatch is the load-bearing entry, called after a message persists and fans
// out. The loop guard lives here: a system/webhook message NEVER triggers an
// agent, and a bot message triggers others only when /autochat is on (and never
// itself). For each matching enabled agent bound to the channel it streams an
// in-channel reply — plus, for a human trigger, opens a forum thread.
func (d *Dispatcher) Dispatch(ctx context.Context, t Trigger) DispatchResult {
	// Per-community switches. nil Policy keeps the historical defaults.
	bots, autochat := true, false
	if d.Policy != nil {
		bots, autochat = d.Policy(ctx, t.CommunityID)
	}
	if !bots {
		return DispatchResult{} // /bots 0 — agents stay silent everywhere
	}

	isBot := t.Kind == chat.KindBot
	switch {
	case isBot:
		// Bot-to-bot is opt-in. A KindBot trigger is the ONLY non-user message
		// allowed to reach an agent, and only when /autochat is on.
		if !autochat {
			return DispatchResult{}
		}
	case t.Kind != chat.KindUser:
		return DispatchResult{} // loop guard: system / webhook / announce never trigger
	}

	agents, err := d.Agents.AgentsForChannel(ctx, t.CommunityID, t.ChannelID)
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("chatagents: agents for channel", "channel", t.ChannelID, "err", err)
		}
		return DispatchResult{}
	}
	if len(agents) == 0 {
		return DispatchResult{}
	}

	multiPrefix := countPrefixAgents(agents) > 1
	var matched []agent.Agent
	for _, a := range agents {
		if isBot && a.ID == t.AuthorAgentID {
			continue // an agent never answers itself (self-mention / "all" mode)
		}
		if Match(a, t.Body, multiPrefix) {
			matched = append(matched, a)
		}
	}
	if len(matched) == 0 {
		return DispatchResult{}
	}

	if isBot {
		// Bot-to-bot: per-agent 15s cooldown instead of the human budget. Drop
		// the agents still cooling down; if all are, nothing runs this turn.
		ready := matched[:0]
		for _, a := range matched {
			if d.allowBotReply(t.ChannelID, a.ID) {
				ready = append(ready, a)
			}
		}
		matched = ready
		if len(matched) == 0 {
			return DispatchResult{}
		}
	} else if d.Gate != nil {
		// Human trigger: one trigger message is one request regardless of how
		// many agents it addresses. Consulted only once a match exists, so plain
		// chatter never consumes a member's budget.
		if dec := d.Gate.Check(ctx, t.CommunityID, t.AuthorID, t.IsSuperAdmin); !dec.Allowed {
			if d.Log != nil {
				d.Log.Info("chatagents: rate limited", "community", t.CommunityID, "user", t.AuthorID, "retry", dec.RetryAfter)
			}
			return DispatchResult{RateLimited: true, RetryAfter: dec.RetryAfter}
		}
	}

	for _, a := range matched {
		// Forum thread — human triggers only (the formal answer, tool trace, and
		// resumable archive). Bot-to-bot stays in the channel: a thread per bot
		// turn would be noise. A thread-create failure is logged but does NOT
		// suppress the in-channel reply below.
		if !isBot && d.CreateThread != nil {
			if threadID, err := d.CreateThread(ctx, t.CommunityID, t.Slug, t.AuthorID, a.ID, a.Name, t.Body); err != nil {
				if d.Log != nil {
					d.Log.Warn("chatagents: create agent thread", "agent", a.ID, "err", err)
				}
			} else if d.Runner != nil {
				d.Runner.Generate(t.CommunityID, threadID, a)
			}
		}
		// In-channel streaming bubble — both human and bot triggers.
		if d.Channel != nil {
			d.Channel.Generate(t.CommunityID, t.ChannelID, t.Slug, a)
		}
	}
	return DispatchResult{}
}
