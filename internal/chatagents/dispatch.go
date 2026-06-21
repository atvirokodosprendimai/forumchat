package chatagents

import (
	"context"
	"log/slog"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
)

// AgentSource supplies the chat-participating agents bound to a channel.
// Satisfied by *agent.Repo.
type AgentSource interface {
	AgentsForChannel(ctx context.Context, communityID, channelID string) ([]agent.Agent, error)
}

// Dispatcher routes a freshly-sent chat message to the agents it triggers.
type Dispatcher struct {
	Agents AgentSource
	Runner *Runner
	Log    *slog.Logger
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(agents AgentSource, runner *Runner, log *slog.Logger) *Dispatcher {
	return &Dispatcher{Agents: agents, Runner: runner, Log: log}
}

// Dispatch is the load-bearing entry: called after a member's message persists
// and fans out. It is a NO-OP unless the triggering message is a human user
// message (kind='user') — the loop guard that makes auto-response safe: a bot,
// webhook, or system message never triggers an agent, so bots never converse
// with each other. For each enabled agent bound to the channel whose trigger
// matches the body, it starts one streaming generation.
func (d *Dispatcher) Dispatch(ctx context.Context, communityID, channelID string, kind chat.Kind, body string) {
	if kind != chat.KindUser {
		return // loop guard
	}
	agents, err := d.Agents.AgentsForChannel(ctx, communityID, channelID)
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("chatagents: agents for channel", "channel", channelID, "err", err)
		}
		return
	}
	if len(agents) == 0 {
		return
	}
	multiPrefix := countPrefixAgents(agents) > 1
	for _, a := range agents {
		if Match(a, body, multiPrefix) {
			d.Runner.Trigger(communityID, channelID, a)
		}
	}
}
