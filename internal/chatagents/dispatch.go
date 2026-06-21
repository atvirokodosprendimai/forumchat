package chatagents

import (
	"context"
	"log/slog"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/agentlimit"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
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
	CommunityID  string
	Slug         string
	ChannelID    string
	AuthorID     string
	AuthorName   string
	Body         string
	Kind         chat.Kind
	IsSuperAdmin bool // exempt from rate limits
}

// Dispatcher routes a freshly-sent chat message to the agents it triggers:
// each match opens a forum thread and streams the agent's first reply there.
type Dispatcher struct {
	Agents       AgentSource
	CreateThread CreateThreadFunc
	Runner       *ThreadRunner
	Gate         RateGate // optional; nil disables rate limiting
	Log          *slog.Logger
}

// NewDispatcher builds a Dispatcher. gate may be nil to disable rate limiting.
func NewDispatcher(agents AgentSource, create CreateThreadFunc, runner *ThreadRunner, gate RateGate, log *slog.Logger) *Dispatcher {
	return &Dispatcher{Agents: agents, CreateThread: create, Runner: runner, Gate: gate, Log: log}
}

// Dispatch is the load-bearing entry: called after a member's message persists
// and fans out. It is a NO-OP unless the triggering message is a human user
// message (kind='user') — the loop guard that keeps a bot/webhook/system
// message from ever triggering an agent. For each enabled agent bound to the
// channel whose trigger matches the body, it creates an agent forum thread and
// streams the agent's first reply into it.
func (d *Dispatcher) Dispatch(ctx context.Context, t Trigger) DispatchResult {
	if t.Kind != chat.KindUser {
		return DispatchResult{} // loop guard
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
		if Match(a, t.Body, multiPrefix) {
			matched = append(matched, a)
		}
	}
	if len(matched) == 0 {
		return DispatchResult{}
	}

	// One trigger message is one request regardless of how many agents it
	// addresses. The gate is consulted only once a match exists, so plain
	// chatter never consumes a member's budget.
	if d.Gate != nil {
		if dec := d.Gate.Check(ctx, t.CommunityID, t.AuthorID, t.IsSuperAdmin); !dec.Allowed {
			if d.Log != nil {
				d.Log.Info("chatagents: rate limited", "community", t.CommunityID, "user", t.AuthorID, "retry", dec.RetryAfter)
			}
			return DispatchResult{RateLimited: true, RetryAfter: dec.RetryAfter}
		}
	}

	for _, a := range matched {
		threadID, err := d.CreateThread(ctx, t.CommunityID, t.Slug, t.AuthorID, a.ID, a.Name, t.Body)
		if err != nil {
			if d.Log != nil {
				d.Log.Warn("chatagents: create agent thread", "agent", a.ID, "err", err)
			}
			continue
		}
		d.Runner.Generate(t.CommunityID, threadID, a)
	}
	return DispatchResult{}
}
