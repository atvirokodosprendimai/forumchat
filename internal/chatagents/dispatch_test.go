package chatagents_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/agentlimit"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/chatagents"
)

// fakeAgents is a static AgentSource.
type fakeAgents struct{ agents []agent.Agent }

func (f fakeAgents) AgentsForChannel(context.Context, string, string) ([]agent.Agent, error) {
	return f.agents, nil
}

// stubGate records calls and returns a canned decision (or one keyed on the
// super-admin flag).
type stubGate struct {
	calls    int
	gotSuper bool
	decision agentlimit.Decision
	bySuper  bool
}

func (g *stubGate) Check(_ context.Context, _, _ string, isSuperAdmin bool) agentlimit.Decision {
	g.calls++
	g.gotSuper = isSuperAdmin
	if g.bySuper {
		return agentlimit.Decision{Allowed: isSuperAdmin}
	}
	return g.decision
}

func allAgent() agent.Agent {
	// TriggerModeAll matches any non-empty body, so every test body triggers.
	return agent.Agent{ID: "a1", Name: "nick", TriggerMode: agent.TriggerModeAll, TriggerPrefix: "."}
}

func userTrigger(body string, super bool) chatagents.Trigger {
	return chatagents.Trigger{
		CommunityID: "c1", Slug: "s", ChannelID: "ch1",
		AuthorID: "u1", AuthorName: "U", Body: body, Kind: chat.KindUser,
		IsSuperAdmin: super,
	}
}

// newDispatcher wires a Dispatcher whose CreateThread increments *created and
// returns createErr (so the nil Runner is never reached when createErr is set).
func newDispatcher(t *testing.T, agents []agent.Agent, gate chatagents.RateGate, created *int, createErr error) *chatagents.Dispatcher {
	t.Helper()
	create := func(context.Context, string, string, string, string, string, string) (string, error) {
		*created++
		return "thread1", createErr
	}
	return chatagents.NewDispatcher(fakeAgents{agents}, create, nil, gate, discard())
}

func TestDispatch_RateLimited(t *testing.T) {
	gate := &stubGate{decision: agentlimit.Decision{Allowed: false, RetryAfter: 12 * time.Second}}
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, gate, &created, nil)

	res := d.Dispatch(context.Background(), userTrigger("anything", false))

	if !res.RateLimited {
		t.Fatal("expected RateLimited")
	}
	if res.RetryAfter != 12*time.Second {
		t.Fatalf("RetryAfter = %v, want 12s", res.RetryAfter)
	}
	if created != 0 {
		t.Fatalf("CreateThread called %d times, want 0 when throttled", created)
	}
}

func TestDispatch_AllowedReachesCreate(t *testing.T) {
	gate := &stubGate{decision: agentlimit.Decision{Allowed: true}}
	created := 0
	// createErr set so the run stops before the (nil) Runner.
	d := newDispatcher(t, []agent.Agent{allAgent()}, gate, &created, errors.New("stop before runner"))

	res := d.Dispatch(context.Background(), userTrigger("anything", false))

	if res.RateLimited {
		t.Fatal("should not be rate-limited when gate allows")
	}
	if created != 1 {
		t.Fatalf("CreateThread called %d times, want 1", created)
	}
	if gate.calls != 1 {
		t.Fatalf("gate consulted %d times, want 1", gate.calls)
	}
}

func TestDispatch_NoMatchSkipsGate(t *testing.T) {
	// A mention-mode agent and a body with no @mention → no match, gate never
	// consulted, no budget consumed.
	mention := agent.Agent{ID: "a1", Name: "nick", TriggerMode: agent.TriggerModeMention, TriggerPrefix: "."}
	gate := &stubGate{decision: agentlimit.Decision{Allowed: false}}
	created := 0
	d := newDispatcher(t, []agent.Agent{mention}, gate, &created, nil)

	res := d.Dispatch(context.Background(), userTrigger("just chatting", false))

	if res.RateLimited {
		t.Fatal("no match must not report rate-limited")
	}
	if gate.calls != 0 {
		t.Fatalf("gate consulted %d times on a non-matching message, want 0", gate.calls)
	}
	if created != 0 {
		t.Fatalf("CreateThread called %d times, want 0", created)
	}
}

func TestDispatch_LoopGuardSkipsBotKind(t *testing.T) {
	gate := &stubGate{decision: agentlimit.Decision{Allowed: false}}
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, gate, &created, nil)

	tr := userTrigger("anything", false)
	tr.Kind = chat.KindBot // not a human message

	res := d.Dispatch(context.Background(), tr)
	if res.RateLimited || gate.calls != 0 || created != 0 {
		t.Fatalf("bot-kind trigger must be a no-op: res=%+v gateCalls=%d created=%d", res, gate.calls, created)
	}
}

func TestDispatch_SuperAdminFlagPassedThrough(t *testing.T) {
	gate := &stubGate{bySuper: true} // allows only when isSuperAdmin
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, gate, &created, errors.New("stop before runner"))

	res := d.Dispatch(context.Background(), userTrigger("anything", true))

	if res.RateLimited {
		t.Fatal("super-admin trigger must pass the gate")
	}
	if !gate.gotSuper {
		t.Fatal("gate did not receive isSuperAdmin=true")
	}
	if created != 1 {
		t.Fatalf("CreateThread called %d times, want 1", created)
	}
}
