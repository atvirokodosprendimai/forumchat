package chatagents_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

// fakeChannel records in-channel reply generations for the bot-to-bot tests.
// Mutex-guarded because paced replies fire on a time.AfterFunc goroutine.
type fakeChannel struct {
	mu     sync.Mutex
	calls  int
	agents []string
}

func (f *fakeChannel) Generate(_, _, _ string, a agent.Agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.agents = append(f.agents, a.ID)
}

func (f *fakeChannel) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// botTrigger is a Kind==KindBot trigger authored by agent authorID — the shape
// the ChannelRunner.OnReply hand-off produces for bot-to-bot.
func botTrigger(body, authorID string) chatagents.Trigger {
	return chatagents.Trigger{
		CommunityID: "c1", Slug: "s", ChannelID: "ch1",
		Body: body, Kind: chat.KindBot, AuthorAgentID: authorID,
	}
}

// twoAgents returns two TriggerModeAll agents so either can answer the other.
func twoAgents() []agent.Agent {
	return []agent.Agent{
		{ID: "a1", Name: "alpha", TriggerMode: agent.TriggerModeAll, TriggerPrefix: "."},
		{ID: "a2", Name: "beta", TriggerMode: agent.TriggerModeAll, TriggerPrefix: "."},
	}
}

func TestDispatch_BotToBot_OffByDefault(t *testing.T) {
	// Policy nil → autochat off: a bot message triggers no one, gate untouched.
	gate := &stubGate{decision: agentlimit.Decision{Allowed: true}}
	created := 0
	d := newDispatcher(t, twoAgents(), gate, &created, nil)
	fc := &fakeChannel{}
	d.Channel = fc

	d.Dispatch(context.Background(), botTrigger("hi alpha", "a2"))

	if fc.calls != 0 {
		t.Fatalf("autochat off: in-channel generate called %d times, want 0", fc.calls)
	}
	if gate.calls != 0 || created != 0 {
		t.Fatalf("autochat off must be inert: gate=%d created=%d", gate.calls, created)
	}
}

func TestDispatch_BotToBot_ExcludesSelfAndStreamsOthers(t *testing.T) {
	created := 0
	d := newDispatcher(t, twoAgents(), &stubGate{}, &created, nil)
	d.Policy = func(context.Context, string) (bool, bool) { return true, true } // autochat on
	fc := &fakeChannel{}
	d.Channel = fc

	// a2 speaks → only a1 should answer (a2 never answers itself), in-channel,
	// and NO forum thread is opened for a bot turn.
	d.Dispatch(context.Background(), botTrigger("anyone there?", "a2"))

	if fc.calls != 1 || len(fc.agents) != 1 || fc.agents[0] != "a1" {
		t.Fatalf("want exactly a1 to reply in-channel, got calls=%d agents=%v", fc.calls, fc.agents)
	}
	if created != 0 {
		t.Fatalf("bot-to-bot must NOT open a forum thread, created=%d", created)
	}
}

func TestDispatch_BotToBot_PacesQueueDoesNotDrop(t *testing.T) {
	created := 0
	d := newDispatcher(t, twoAgents(), &stubGate{}, &created, nil)
	d.Policy = func(context.Context, string) (bool, bool) { return true, true }
	d.Cooldown = 40 * time.Millisecond // tiny window so the test is fast
	fc := &fakeChannel{}
	d.Channel = fc

	// Three bot messages from a2 in quick succession. a1 answers the first
	// immediately; the next two land inside a1's cooldown window and must
	// COALESCE into exactly ONE queued reply (not be dropped, not stack to 3).
	d.Dispatch(context.Background(), botTrigger("ping 1", "a2"))
	d.Dispatch(context.Background(), botTrigger("ping 2", "a2"))
	d.Dispatch(context.Background(), botTrigger("ping 3", "a2"))

	if got := fc.Count(); got != 1 {
		t.Fatalf("immediately: want 1 reply (window open once), got %d", got)
	}
	// After the window elapses the single queued reply fires.
	time.Sleep(90 * time.Millisecond)
	if got := fc.Count(); got != 2 {
		t.Fatalf("after cooldown: want 2 (1 immediate + 1 queued), got %d — queue dropped or stacked", got)
	}
}

func TestDispatch_BotToBot_QueuedReplyRespectsKill(t *testing.T) {
	created := 0
	d := newDispatcher(t, twoAgents(), &stubGate{}, &created, nil)
	// atomic: the Policy closure is read from the paced-reply goroutine while the
	// test goroutine flips it (mirrors the real DB-backed Policy being concurrent).
	var autochat atomic.Bool
	autochat.Store(true)
	d.Policy = func(context.Context, string) (bool, bool) { return true, autochat.Load() }
	d.Cooldown = 40 * time.Millisecond
	fc := &fakeChannel{}
	d.Channel = fc

	d.Dispatch(context.Background(), botTrigger("ping 1", "a2")) // a1 fires now
	d.Dispatch(context.Background(), botTrigger("ping 2", "a2")) // a1 queued
	autochat.Store(false)                                       // admin /kill during the wait
	time.Sleep(90 * time.Millisecond)
	if got := fc.Count(); got != 1 {
		t.Fatalf("a queued reply must re-check the switch and NOT fire after /kill, got %d", got)
	}
}

func TestDispatch_BotsMasterOff_Silences(t *testing.T) {
	gate := &stubGate{decision: agentlimit.Decision{Allowed: true}}
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, gate, &created, nil)
	d.Policy = func(context.Context, string) (bool, bool) { return false, false } // /bots 0
	fc := &fakeChannel{}
	d.Channel = fc

	res := d.Dispatch(context.Background(), userTrigger("@nick hi", false))

	if fc.calls != 0 || created != 0 || gate.calls != 0 || res.RateLimited {
		t.Fatalf("/bots 0 must silence everything: ch=%d created=%d gate=%d limited=%v",
			fc.calls, created, gate.calls, res.RateLimited)
	}
}

func TestDispatch_HumanTrigger_BothSurfaces(t *testing.T) {
	// A human trigger opens a forum thread AND streams an in-channel bubble.
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, &stubGate{decision: agentlimit.Decision{Allowed: true}}, &created, nil)
	d.Policy = func(context.Context, string) (bool, bool) { return true, false }
	fc := &fakeChannel{}
	d.Channel = fc

	d.Dispatch(context.Background(), userTrigger("@nick hi", false))

	if created != 1 {
		t.Fatalf("human trigger should open one forum thread, created=%d", created)
	}
	if fc.calls != 1 {
		t.Fatalf("human trigger should also stream one in-channel reply, calls=%d", fc.calls)
	}
}

func TestDispatch_Surface_ChannelOnly(t *testing.T) {
	// Surface "channel" → in-channel bubble only, NO forum thread.
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, &stubGate{decision: agentlimit.Decision{Allowed: true}}, &created, nil)
	d.Policy = func(context.Context, string) (bool, bool) { return true, false }
	d.Surface = func(context.Context, string) string { return chatagents.SurfaceChannel }
	fc := &fakeChannel{}
	d.Channel = fc

	d.Dispatch(context.Background(), userTrigger("@nick hi", false))

	if created != 0 {
		t.Fatalf("channel-only surface must NOT open a forum thread, created=%d", created)
	}
	if fc.calls != 1 {
		t.Fatalf("channel-only surface must stream one in-channel reply, calls=%d", fc.calls)
	}
}

func TestDispatch_Surface_ThreadOnly(t *testing.T) {
	// Surface "thread" → forum thread only, NO in-channel bubble.
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, &stubGate{decision: agentlimit.Decision{Allowed: true}}, &created, nil)
	d.Policy = func(context.Context, string) (bool, bool) { return true, false }
	d.Surface = func(context.Context, string) string { return chatagents.SurfaceThread }
	fc := &fakeChannel{}
	d.Channel = fc

	d.Dispatch(context.Background(), userTrigger("@nick hi", false))

	if created != 1 {
		t.Fatalf("thread-only surface must open one forum thread, created=%d", created)
	}
	if fc.calls != 0 {
		t.Fatalf("thread-only surface must NOT stream an in-channel reply, calls=%d", fc.calls)
	}
}

func TestDispatch_Surface_UnknownDefaultsBoth(t *testing.T) {
	// A bad/empty surface value degrades to "both" — a misconfig never silences.
	created := 0
	d := newDispatcher(t, []agent.Agent{allAgent()}, &stubGate{decision: agentlimit.Decision{Allowed: true}}, &created, nil)
	d.Policy = func(context.Context, string) (bool, bool) { return true, false }
	d.Surface = func(context.Context, string) string { return "garbage" }
	fc := &fakeChannel{}
	d.Channel = fc

	d.Dispatch(context.Background(), userTrigger("@nick hi", false))

	if created != 1 || fc.calls != 1 {
		t.Fatalf("unknown surface must fall back to both, created=%d calls=%d", created, fc.calls)
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
