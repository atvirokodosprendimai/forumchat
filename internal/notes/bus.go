package notes

import "sync"

// Bus is an in-process per-note fan-out for collaborative editing. Each open
// editor's collab SSE stream Subscribes by note id; a merged edit (or a Save)
// Broadcasts on that id and only that note's subscribers wake to re-sync. NATS
// handles cross-process fan-out; this Bus handles same-process multi-tab. Mirrors
// lobbies.Bus (§4.11).
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[string]map[chan struct{}]struct{}{}} }

// Subscribe registers for events on a single note. Returns the channel + an
// unsubscribe func.
func (b *Bus) Subscribe(noteID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	set, ok := b.subs[noteID]
	if !ok {
		set = map[chan struct{}]struct{}{}
		b.subs[noteID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if set, ok := b.subs[noteID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(b.subs, noteID)
			}
		}
		b.mu.Unlock()
	}
}

// Broadcast pings every subscriber on noteID. Idempotent — a subscriber that
// hasn't drained its previous ping is left at "1 pending" (refetch semantics).
func (b *Bus) Broadcast(noteID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[noteID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
