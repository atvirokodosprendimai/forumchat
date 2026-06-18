package chat

import "sync"

// Bus is an in-process fan-out. Chat write paths (send/delete/promote)
// call Bus.Broadcast(channelID) after persistence; every open SSE stream
// in this process receives the changed channel id and decides what to do
// with it (fat-morph if it's the viewer's active channel, set an unread
// dot otherwise).
//
// NATS handles cross-process fan-out. Bus handles same-process multi-tab
// fan-out and keeps the UI realtime even when NATS is unreachable.
type Bus struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func NewBus() *Bus { return &Bus{subs: map[chan string]struct{}{}} }

// Subscribe registers a new subscriber and returns its channel + an
// unsubscribe. The channel is buffered so distinct channel ids broadcast
// in quick succession aren't coalesced into one (each open stream needs
// to see every changed channel to keep its unread dots correct). A full
// buffer drops the oldest-style — best-effort; a missed ping is corrected
// by the next event or the page-load UnreadChannels seed.
func (b *Bus) Subscribe() (<-chan string, func()) {
	ch := make(chan string, 16)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Broadcast delivers channelID to every current subscriber. Non-blocking
// — a subscriber whose buffer is full drops this ping.
func (b *Bus) Broadcast(channelID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- channelID:
		default:
		}
	}
}
