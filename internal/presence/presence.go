package presence

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

type Member struct {
	UserID       string
	DisplayName  string
	AvatarURL    string
	LastSeen     time.Time
}

type Tracker struct {
	mu      sync.Mutex
	ttl     time.Duration
	by      map[string]map[string]Member // communityID -> userID -> Member
	changed map[string]chan struct{}     // communityID -> notify on change
}

func New(ttl time.Duration) *Tracker {
	return &Tracker{
		ttl:     ttl,
		by:      make(map[string]map[string]Member),
		changed: make(map[string]chan struct{}),
	}
}

func (t *Tracker) Touch(communityID string, m Member) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.by[communityID]
	if !ok {
		c = make(map[string]Member)
		t.by[communityID] = c
	}
	m.LastSeen = time.Now()
	prev, existed := c[m.UserID]
	c[m.UserID] = m
	if !existed || prev.DisplayName != m.DisplayName || prev.AvatarURL != m.AvatarURL {
		t.notify(communityID)
	}
}

func (t *Tracker) Members(communityID string) []Member {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := t.by[communityID]
	now := time.Now()
	out := make([]Member, 0, len(c))
	for uid, m := range c {
		if now.Sub(m.LastSeen) > t.ttl {
			delete(c, uid)
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DisplayName < out[j].DisplayName })
	return out
}

// Changed returns a channel that receives a token whenever this community's
// presence set changes. The returned func unregisters the watcher.
func (t *Tracker) Watch(communityID string) (<-chan struct{}, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan struct{}, 1)
	key := communityID + "/" + randomKey()
	if t.changed == nil {
		t.changed = make(map[string]chan struct{})
	}
	t.changed[key] = ch
	cancel := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if c, ok := t.changed[key]; ok {
			close(c)
			delete(t.changed, key)
		}
	}
	return ch, cancel
}

// Bump wakes every watcher for the community without changing presence
// state. Used by admin mutations (role change, ban) so open chat rosters
// re-render against the fresh DB rows immediately.
func (t *Tracker) Bump(communityID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notify(communityID)
}

func (t *Tracker) notify(communityID string) {
	for key, ch := range t.changed {
		if len(key) < len(communityID)+1 || key[:len(communityID)] != communityID {
			continue
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Sweep removes stale entries and notifies watchers. Run on a timer.
func (t *Tracker) Sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for cid, m := range t.by {
		removed := false
		for uid, mem := range m {
			if now.Sub(mem.LastSeen) > t.ttl {
				delete(m, uid)
				removed = true
			}
		}
		if removed {
			t.notify(cid)
		}
	}
}

// randomKey returns a collision-free subscriber key. It was derived from
// time.Now().UnixNano(), so two Watch calls in the same nanosecond produced the
// same key — the second overwrote the first in the subscriber map, silently
// dropping a roster stream and leaking its goroutine. Use crypto/rand instead.
func randomKey() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// rand.Read never fails on supported platforms; fall back to a
		// time-based key rather than panicking in a presence helper.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b)
}
