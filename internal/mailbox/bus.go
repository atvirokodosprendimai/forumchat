package mailbox

import "sync"

// Bus is the in-process per-community fan-out for inbox SSE streams.
// When the poll worker writes a new ingest row, it Broadcasts on the
// community ID; every viewer's SSE loop subscribed to that community
// re-runs its read query and patches the inbox list.
//
// Cross-process fan-out is handled by NATS (subject
// community.<cid>.mailbox) which the wiring in cmd/app/main.go
// publishes to alongside this Bus.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

// NewBus returns an empty Bus.
func NewBus() *Bus { return &Bus{subs: map[string]map[chan struct{}]struct{}{}} }

// Subscribe registers for events on a single community. Returns the
// channel + an unsubscribe func the caller MUST defer.
func (b *Bus) Subscribe(communityID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	set, ok := b.subs[communityID]
	if !ok {
		set = map[chan struct{}]struct{}{}
		b.subs[communityID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if set, ok := b.subs[communityID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.subs, communityID)
			}
		}
		b.mu.Unlock()
	}
}

// Broadcast pings every subscriber on communityID. Idempotent: a
// subscriber that hasn't drained its previous ping is left at "1
// pending" — the reader refetches once when it next selects.
func (b *Bus) Broadcast(communityID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[communityID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
