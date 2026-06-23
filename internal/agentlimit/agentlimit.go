// Package agentlimit rate-limits AI-agent prompt requests per community.
//
// One in-process sliding-window Limiter counts triggers over a fixed window
// (one minute) under two keys per community: per-user (community-wide) and
// per-community (all members combined). A Gate pairs the Limiter with a
// per-community limits lookup so callers ask one question — "may this member
// trigger an agent now?" — and get a Decision back.
//
// In-process only: counters live in this process, consistent with the chat Bus
// and presence tracker. A multi-process (NATS-fanned) deployment does not share
// a counter, so each node enforces the limit independently.
package agentlimit

import (
	"context"
	"sync"
	"time"
)

// Window is the rolling period the per-minute limits are measured over.
const Window = time.Minute

// Limiter is a thread-safe sliding-window counter keyed by arbitrary strings.
// The zero value is not usable; call New.
type Limiter struct {
	mu     sync.Mutex
	events map[string][]time.Time
	now    func() time.Time // injectable for tests
}

// New builds an empty Limiter using the wall clock.
func New() *Limiter {
	return &Limiter{events: make(map[string][]time.Time), now: time.Now}
}

// prune drops timestamps older than the window for key and returns the live
// slice. Caller holds l.mu.
func (l *Limiter) prune(key string, now time.Time) []time.Time {
	cutoff := now.Add(-Window)
	ts := l.events[key]
	keep := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.events, key)
		return nil
	}
	l.events[key] = keep
	return keep
}

// allow checks whether key is under limit without recording. limit<=0 means
// unlimited. Returns ok and, on denial, how long until the oldest in-window
// event ages out. Caller holds l.mu.
func (l *Limiter) allow(key string, limit int, now time.Time) (bool, time.Duration) {
	if limit <= 0 {
		return true, 0
	}
	ts := l.prune(key, now)
	if len(ts) < limit {
		return true, 0
	}
	retry := max(ts[0].Add(Window).Sub(now), time.Second)
	return false, retry
}

// record appends now to key's window. Caller holds l.mu.
func (l *Limiter) record(key string, now time.Time) {
	l.events[key] = append(l.events[key], now)
}

// AllowRecord atomically checks whether key is under limit in the current
// window and, if so, records this event. limit<=0 means unlimited. On denial it
// returns how long until the oldest in-window event ages out. The check and the
// record happen under one lock, so concurrent callers can't both slip past the
// limit. Exported for reuse by non-agent callers (e.g. chat flood control).
func (l *Limiter) AllowRecord(key string, limit int) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	ok, retry := l.allow(key, limit, now)
	if ok {
		l.record(key, now)
	}
	return ok, retry
}

// Decision is the result of a Gate check.
type Decision struct {
	Allowed    bool
	RetryAfter time.Duration // valid when !Allowed
}

// Limits are a community's per-minute caps (0 = unlimited).
type Limits struct {
	PerUserMin      int
	PerCommunityMin int
}

// LimitsFunc resolves the caps for a community. Wired in main.go to
// community.Repo so agentlimit needn't import community.
type LimitsFunc func(ctx context.Context, communityID string) (Limits, error)

// Gate decides whether a member may trigger an agent right now and records the
// trigger when allowed. It pairs a Limiter with a per-community limits lookup.
type Gate struct {
	lim    *Limiter
	limits LimitsFunc
}

// NewGate builds a Gate over a fresh Limiter. limits must be non-nil.
func NewGate(limits LimitsFunc) *Gate {
	return &Gate{lim: New(), limits: limits}
}

// Check reports whether userID may trigger an agent in communityID now. A
// super-admin always passes and is not counted. Both the per-user and the
// per-community caps must have headroom; the trigger is recorded against both
// only when both pass (a denied request consumes no budget). When the limits
// lookup fails, the request is allowed (fail-open) — rate limiting must never
// take agents down.
func (g *Gate) Check(ctx context.Context, communityID, userID string, isSuperAdmin bool) Decision {
	if isSuperAdmin {
		return Decision{Allowed: true}
	}
	lm, err := g.limits(ctx, communityID)
	if err != nil {
		return Decision{Allowed: true}
	}
	if lm.PerUserMin <= 0 && lm.PerCommunityMin <= 0 {
		return Decision{Allowed: true}
	}

	userKey := "u:" + communityID + ":" + userID
	commKey := "c:" + communityID

	g.lim.mu.Lock()
	defer g.lim.mu.Unlock()

	now := g.lim.now()
	if ok, retry := g.lim.allow(userKey, lm.PerUserMin, now); !ok {
		return Decision{RetryAfter: retry}
	}
	if ok, retry := g.lim.allow(commKey, lm.PerCommunityMin, now); !ok {
		return Decision{RetryAfter: retry}
	}
	g.lim.record(userKey, now)
	g.lim.record(commKey, now)
	return Decision{Allowed: true}
}
