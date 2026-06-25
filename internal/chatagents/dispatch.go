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

// Bot reply surfaces — where a triggered agent answers, stored per community
// (communities.agents_reply_surface) and resolved via Dispatcher.Surface. An
// unrecognised value is treated as SurfaceBoth so a bad config degrades to the
// historical behaviour rather than silencing agents.
const (
	SurfaceChannel = "channel" // in-channel kind='bot' bubble only, no thread
	SurfaceThread  = "thread"  // forum thread (+ chat announce) only, no bubble
	SurfaceBoth    = "both"    // forum thread AND in-channel bubble (default)
)

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

// ChannelReplier streams an agent's reply as an in-channel bubble. Satisfied by
// *ChannelRunner; an interface so the dispatch decision logic (bot-to-bot
// gating, self-exclusion, the 15s cooldown) is testable without a live model.
type ChannelReplier interface {
	Generate(communityID, channelID, slug string, a agent.Agent)
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
// human send answers on the community's chosen surface (Surface): a forum thread
// (the formal, resumable answer), an in-channel bubble, or both (default). A
// bot's own reply (when /autochat is on) re-enters here and streams an
// in-channel bubble only — keeping bot-to-bot banter in the channel, never
// spawning a thread per turn regardless of Surface.
type Dispatcher struct {
	Agents       AgentSource
	CreateThread CreateThreadFunc
	Runner       *ThreadRunner  // forum-thread streamer (human triggers)
	Channel      ChannelReplier // in-channel streamer (both human + bot); nil disables in-channel replies
	Gate         RateGate       // optional; nil disables human rate limiting
	Policy       PolicyFunc     // optional; nil → bots on, autochat off
	// Surface reports a community's bot reply surface (SurfaceChannel /
	// SurfaceThread / SurfaceBoth) for a HUMAN trigger. nil → SurfaceBoth,
	// preserving the historical both-surfaces behaviour (and keeping the
	// forum-thread unit tests, which set no Surface, unchanged). Bot-to-bot
	// turns are always channel-only regardless of this setting.
	Surface func(ctx context.Context, communityID string) string
	Log     *slog.Logger

	// Cooldown overrides BotReplyCooldown (tests set it small). 0 → the const.
	Cooldown time.Duration

	// botMu guards the bot-to-bot pacing state. botLast is each (channel,agent)'s
	// last reply START; botPending marks that a paced reply is already scheduled
	// for that key, so a flurry of triggers during the window coalesces into ONE
	// queued reply instead of being dropped (the user's "queue, don't drop"). All
	// process-local — a cost/pacing throttle, not a correctness gate.
	botMu      sync.Mutex
	botLast    map[string]time.Time
	botPending map[string]bool
}

// NewDispatcher builds a Dispatcher. gate may be nil to disable rate limiting.
// Channel and Policy are set by the caller after construction (main.go) — left
// nil they preserve the forum-thread-only, always-on behaviour.
func NewDispatcher(agents AgentSource, create CreateThreadFunc, runner *ThreadRunner, gate RateGate, log *slog.Logger) *Dispatcher {
	return &Dispatcher{
		Agents: agents, CreateThread: create, Runner: runner, Gate: gate, Log: log,
		botLast: map[string]time.Time{}, botPending: map[string]bool{},
	}
}

// cooldown is the effective per-agent bot-to-bot gap.
func (d *Dispatcher) cooldown() time.Duration {
	if d.Cooldown > 0 {
		return d.Cooldown
	}
	return BotReplyCooldown
}

// scheduleBotReplies paces bot-to-bot: for each matched agent, fire its reply
// now if the cooldown window is open, otherwise QUEUE exactly one reply at the
// window's end (coalescing further triggers for that agent). This is what keeps
// a two-bot conversation alive — a turn that arrives mid-cooldown is delayed,
// not dropped — while still capping each agent to one reply per window.
func (d *Dispatcher) scheduleBotReplies(communityID, slug, channelID string, agents []agent.Agent) {
	cd := d.cooldown()
	now := time.Now()
	for _, a := range agents {
		key := channelID + "|" + a.ID
		d.botMu.Lock()
		last, seen := d.botLast[key]
		switch {
		case !seen || now.Sub(last) >= cd:
			// Window open → reply now, stamp the start.
			d.botLast[key] = now
			d.botMu.Unlock()
			if d.Channel != nil {
				d.Channel.Generate(communityID, channelID, slug, a)
			}
		case d.botPending[key]:
			// A reply is already queued for this agent — coalesce.
			d.botMu.Unlock()
		default:
			// Window closed → queue one reply for when it opens.
			d.botPending[key] = true
			wait := cd - now.Sub(last)
			ag := a
			d.botMu.Unlock()
			time.AfterFunc(wait, func() { d.firePacedReply(communityID, slug, channelID, ag) })
		}
	}
}

// firePacedReply runs a queued bot reply when its cooldown window opens. It
// re-checks the switches first — an admin may have /killed autochat (or run
// /bots 0) during the wait — so a queued turn never fires against a disabled
// community.
func (d *Dispatcher) firePacedReply(communityID, slug, channelID string, a agent.Agent) {
	key := channelID + "|" + a.ID
	d.botMu.Lock()
	delete(d.botPending, key)
	d.botLast[key] = time.Now()
	d.botMu.Unlock()
	if d.Policy != nil {
		if bots, autochat := d.Policy(context.Background(), communityID); !bots || !autochat {
			return
		}
	}
	if d.Channel != nil {
		d.Channel.Generate(communityID, channelID, slug, a)
	}
}

// Dispatch is the load-bearing entry, called after a message persists and fans
// out. The loop guard lives here: a system/webhook message NEVER triggers an
// agent, and a bot message triggers others only when /autochat is on (and never
// itself). For each matching enabled agent bound to the channel it answers on
// the community's chosen surface: an in-channel reply, a forum thread, or both
// for a human trigger (bot-to-bot turns are always in-channel only).
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
		// Bot-to-bot: pace each matched agent (≤1 reply per cooldown window),
		// QUEUEING a mid-window turn rather than dropping it so the conversation
		// keeps flowing. Channel-only — no forum thread per bot turn.
		d.scheduleBotReplies(t.CommunityID, t.Slug, t.ChannelID, matched)
		return DispatchResult{}
	}

	// Human trigger: one trigger message is one request regardless of how many
	// agents it addresses. The gate is consulted only once a match exists, so
	// plain chatter never consumes a member's budget.
	if d.Gate != nil {
		if dec := d.Gate.Check(ctx, t.CommunityID, t.AuthorID, t.IsSuperAdmin); !dec.Allowed {
			if d.Log != nil {
				d.Log.Info("chatagents: rate limited", "community", t.CommunityID, "user", t.AuthorID, "retry", dec.RetryAfter)
			}
			return DispatchResult{RateLimited: true, RetryAfter: dec.RetryAfter}
		}
	}

	// Resolve where the agents answer for this community. An unknown/empty value
	// degrades to SurfaceBoth — a misconfiguration must never silence agents.
	surface := SurfaceBoth
	if d.Surface != nil {
		if s := d.Surface(ctx, t.CommunityID); s == SurfaceChannel || s == SurfaceThread {
			surface = s
		}
	}

	for _, a := range matched {
		// Forum thread — when the surface is "thread" or "both" (the formal
		// answer, tool trace, and resumable archive). A thread-create failure is
		// logged but does NOT suppress the in-channel reply below.
		if surface != SurfaceChannel && d.CreateThread != nil {
			if threadID, err := d.CreateThread(ctx, t.CommunityID, t.Slug, t.AuthorID, a.ID, a.Name, t.Body); err != nil {
				if d.Log != nil {
					d.Log.Warn("chatagents: create agent thread", "agent", a.ID, "err", err)
				}
			} else if d.Runner != nil {
				d.Runner.Generate(t.CommunityID, threadID, a)
			}
		}
		// In-channel streaming bubble — when the surface is "channel" or "both".
		if surface != SurfaceThread && d.Channel != nil {
			d.Channel.Generate(t.CommunityID, t.ChannelID, t.Slug, a)
		}
	}
	return DispatchResult{}
}
