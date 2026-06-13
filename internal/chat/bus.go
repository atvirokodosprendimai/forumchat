package chat

import "sync"

// Bus is an in-process fan-out. Chat write paths (send/delete/promote) call
// Bus.Broadcast after persistence; every open SSE stream in this process
// receives the signal and refetches.
//
// NATS handles cross-process fan-out. Bus handles same-process multi-tab
// fan-out and keeps the UI realtime even when NATS is unreachable.
type Bus struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[chan struct{}]struct{}{}} }

// Subscribe registers a new subscriber and returns its channel + an unsubscribe.
// The channel buffers a single pending broadcast — subsequent broadcasts before
// the consumer drains are coalesced.
func (b *Bus) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Broadcast pings every current subscriber. Non-blocking — a subscriber that
// hasn't drained its previous ping is left at "1 pending" (idempotent for
// refetch-style consumers like the chat SSE handler).
func (b *Bus) Broadcast() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
