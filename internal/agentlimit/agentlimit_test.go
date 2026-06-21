package agentlimit

import (
	"context"
	"testing"
	"time"
)

// fakeClock lets tests advance time deterministically.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

// newGate builds a Gate with a fixed limits lookup and an injected clock.
func newGate(lm Limits, clk *fakeClock) *Gate {
	g := NewGate(func(context.Context, string) (Limits, error) { return lm, nil })
	g.lim.now = clk.now
	return g
}

func TestGate_PerUserWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newGate(Limits{PerUserMin: 3, PerCommunityMin: 0}, clk)
	ctx := context.Background()

	for i := range 3 {
		if d := g.Check(ctx, "c1", "u1", false); !d.Allowed {
			t.Fatalf("trigger %d should be allowed", i+1)
		}
	}
	d := g.Check(ctx, "c1", "u1", false)
	if d.Allowed {
		t.Fatal("4th trigger in window should be denied")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > Window {
		t.Fatalf("retry-after out of range: %v", d.RetryAfter)
	}

	// A different user in the same community is unaffected.
	if d := g.Check(ctx, "c1", "u2", false); !d.Allowed {
		t.Fatal("other user should have own budget")
	}

	// After the window elapses, u1 is allowed again.
	clk.add(Window + time.Second)
	if d := g.Check(ctx, "c1", "u1", false); !d.Allowed {
		t.Fatal("trigger should be allowed after window")
	}
}

func TestGate_PerCommunityCap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	// High per-user, low community cap: the community cap binds across users.
	g := newGate(Limits{PerUserMin: 100, PerCommunityMin: 2}, clk)
	ctx := context.Background()

	if d := g.Check(ctx, "c1", "u1", false); !d.Allowed {
		t.Fatal("first community trigger allowed")
	}
	if d := g.Check(ctx, "c1", "u2", false); !d.Allowed {
		t.Fatal("second community trigger allowed")
	}
	if d := g.Check(ctx, "c1", "u3", false); d.Allowed {
		t.Fatal("third trigger should hit community cap")
	}
}

func TestGate_SuperAdminBypass(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newGate(Limits{PerUserMin: 1, PerCommunityMin: 1}, clk)
	ctx := context.Background()

	for i := range 50 {
		if d := g.Check(ctx, "c1", "admin", true); !d.Allowed {
			t.Fatalf("super-admin must never be limited (i=%d)", i)
		}
	}
	// Super-admin triggers must not consume the community budget either.
	if d := g.Check(ctx, "c1", "u1", false); !d.Allowed {
		t.Fatal("regular user budget should be untouched by super-admin traffic")
	}
}

func TestGate_Unlimited(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	g := newGate(Limits{PerUserMin: 0, PerCommunityMin: 0}, clk)
	ctx := context.Background()
	for i := range 100 {
		if d := g.Check(ctx, "c1", "u1", false); !d.Allowed {
			t.Fatalf("0 limits mean unlimited (i=%d)", i)
		}
	}
}

func TestGate_DeniedConsumesNoBudget(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	// User cap 5, community cap 1: u1 takes the only community slot, so u2 is
	// denied by the community cap. That denial must not consume u2's per-user
	// budget — observed directly on the limiter's internal counters.
	g := newGate(Limits{PerUserMin: 5, PerCommunityMin: 1}, clk)
	ctx := context.Background()

	if d := g.Check(ctx, "c1", "u1", false); !d.Allowed {
		t.Fatal("u1 first allowed")
	}
	if d := g.Check(ctx, "c1", "u2", false); d.Allowed {
		t.Fatal("u2 must be denied by community cap")
	}
	if n := len(g.lim.events["u:c1:u2"]); n != 0 {
		t.Fatalf("community-denied call consumed u2 budget: got %d events, want 0", n)
	}
}
