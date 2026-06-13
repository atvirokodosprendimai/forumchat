package forum

import "sync"

// Bus is an in-process per-thread fan-out. Forum write paths (reply, delete)
// call Bus.Broadcast(threadID) after persistence; SSE streams subscribed to
// that thread receive the signal and refetch.
//
// NATS handles cross-process fan-out via a per-thread subject. Bus handles
// same-process multi-tab fan-out and keeps the UI realtime even when NATS
// is unreachable.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[string]map[chan struct{}]struct{}{}} }

// Subscribe registers a subscriber for threadID and returns its channel plus
// an unsubscribe func. The channel buffers one pending broadcast — subsequent
// broadcasts before the consumer drains are coalesced.
func (b *Bus) Subscribe(threadID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	m, ok := b.subs[threadID]
	if !ok {
		m = map[chan struct{}]struct{}{}
		b.subs[threadID] = m
	}
	m[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if m, ok := b.subs[threadID]; ok {
			delete(m, ch)
			if len(m) == 0 {
				delete(b.subs, threadID)
			}
		}
		b.mu.Unlock()
	}
}

// Broadcast pings every subscriber on threadID. Non-blocking.
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
