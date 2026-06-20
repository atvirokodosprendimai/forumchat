package agent

import "sync"

// Bus is an in-process per-thread fan-out. SSE streams subscribe by thread
// id; the generation runner (and write handlers) Broadcast on that id; only
// the subscribers for the matching thread wake. NATS handles cross-process
// fan-out; this Bus handles same-process multi-tab (the same thread open in
// two windows, or a shared thread watched by several members on one node).
// Mirrors lobbies.Bus.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[string]map[chan struct{}]struct{}{}} }

// Subscribe registers for events on a single thread. Returns the channel + an
// unsubscribe func.
func (b *Bus) Subscribe(threadID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	set, ok := b.subs[threadID]
	if !ok {
		set = map[chan struct{}]struct{}{}
		b.subs[threadID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if set, ok := b.subs[threadID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.subs, threadID)
			}
		}
		b.mu.Unlock()
	}
}

// Broadcast pings every subscriber on threadID. Idempotent — a subscriber that
// hasn't drained its previous ping is left at "1 pending", matching the
// refetch-from-DB semantics every stream uses.
func (b *Bus) Broadcast(threadID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[threadID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
