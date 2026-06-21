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
}

// Dispatcher routes a freshly-sent chat message to the agents it triggers:
// each match opens a forum thread and streams the agent's first reply there.
type Dispatcher struct {
	Agents       AgentSource
	CreateThread CreateThreadFunc
	Runner       *ThreadRunner
	Log          *slog.Logger
}

// NewDispatcher builds a Dispatcher.
func NewDispatcher(agents AgentSource, create CreateThreadFunc, runner *ThreadRunner, log *slog.Logger) *Dispatcher {
	return &Dispatcher{Agents: agents, CreateThread: create, Runner: runner, Log: log}
}

// Dispatch is the load-bearing entry: called after a member's message persists
// and fans out. It is a NO-OP unless the triggering message is a human user
// message (kind='user') — the loop guard that keeps a bot/webhook/system
// message from ever triggering an agent. For each enabled agent bound to the
// channel whose trigger matches the body, it creates an agent forum thread and
// streams the agent's first reply into it.
func (d *Dispatcher) Dispatch(ctx context.Context, t Trigger) {
	if t.Kind != chat.KindUser {
		return // loop guard
	}
	agents, err := d.Agents.AgentsForChannel(ctx, t.CommunityID, t.ChannelID)
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("chatagents: agents for channel", "channel", t.ChannelID, "err", err)
		}
		return
	}
	if len(agents) == 0 {
		return
	}
	multiPrefix := countPrefixAgents(agents) > 1
	for _, a := range agents {
		if !Match(a, t.Body, multiPrefix) {
			continue
		}
		threadID, err := d.CreateThread(ctx, t.CommunityID, t.Slug, t.AuthorID, a.ID, a.Name, t.Body)
		if err != nil {
			if d.Log != nil {
				d.Log.Warn("chatagents: create agent thread", "agent", a.ID, "err", err)
			}
			continue
		}
		d.Runner.Generate(t.CommunityID, threadID, a)
	}
}
