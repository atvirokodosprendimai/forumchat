package lobbies

import "sync"

// Bus is an in-process per-lobby fan-out. SSE streams subscribe by
// lobby_id; Send / Close handlers Broadcast on that id; only the
// subscribers for the matching lobby wake. NATS handles cross-process
// fanout; this Bus handles same-process multi-tab (host has lobby open
// in two windows, or host + guest tabs on the same machine).
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[string]map[chan struct{}]struct{}{}} }

// Subscribe registers for events on a single lobby. Returns the channel
// + an unsubscribe func.
func (b *Bus) Subscribe(lobbyID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	set, ok := b.subs[lobbyID]
	if !ok {
		set = map[chan struct{}]struct{}{}
		b.subs[lobbyID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if set, ok := b.subs[lobbyID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.subs, lobbyID)
			}
		}
		b.mu.Unlock()
	}
}

// Broadcast pings every subscriber on `lobbyID`. Idempotent — a
// subscriber that hasn't drained its previous ping is left at "1
// pending", matching chat.Bus's refetch-style semantics.
func (b *Bus) Broadcast(lobbyID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[lobbyID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
