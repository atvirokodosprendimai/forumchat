package rooms

import "sync"

// Event is a single thing pushed to all subscribers of a room (presence
// change, chat, room meta update, etc.). Signaling traffic is NOT broadcast
// — see SignalEnvelope below for the per-peer fan-out.
type Event struct {
	Kind string // "presence" | "chat" | "meta" | "approval"
}

// SignalEnvelope is one routed signaling payload, addressed at a specific
// participant key (Identity.Key()).
type SignalEnvelope struct {
	FromKey string
	Kind    string // "offer" | "answer" | "ice" | "bye"
	Payload string // opaque JSON forwarded verbatim to the recipient
}

// Bus is the per-room fan-out hub. Two channel types:
//   - room-wide events (everyone in the room subscribes)
//   - per-peer signaling mailboxes (one inbox per participant key)
type Bus struct {
	mu       sync.Mutex
	roomSubs map[string]map[chan Event]struct{}            // roomID -> set
	sigSubs  map[string]map[string]chan SignalEnvelope     // roomID -> key -> ch
}

func NewBus() *Bus {
	return &Bus{
		roomSubs: map[string]map[chan Event]struct{}{},
		sigSubs:  map[string]map[string]chan SignalEnvelope{},
	}
}

// SubscribeRoom registers a channel to receive room-wide events.
func (b *Bus) SubscribeRoom(roomID string) (<-chan Event, func()) {
	ch := make(chan Event, 8)
	b.mu.Lock()
	set := b.roomSubs[roomID]
	if set == nil {
		set = map[chan Event]struct{}{}
		b.roomSubs[roomID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if s := b.roomSubs[roomID]; s != nil {
			delete(s, ch)
			if len(s) == 0 {
				delete(b.roomSubs, roomID)
			}
		}
		b.mu.Unlock()
	}
}

// PublishRoom delivers an Event to every room subscriber. Non-blocking;
// drops on full buffers (SSE handler will resync on next event).
func (b *Bus) PublishRoom(roomID string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.roomSubs[roomID] {
		select {
		case ch <- ev:
		default:
		}
	}
}

// SubscribeSignal registers a per-peer signaling mailbox keyed by the
// participant's Identity.Key(). Replaces any prior mailbox under that key
// (so reconnects don't leak).
func (b *Bus) SubscribeSignal(roomID, key string) (<-chan SignalEnvelope, func()) {
	ch := make(chan SignalEnvelope, 32)
	b.mu.Lock()
	if b.sigSubs[roomID] == nil {
		b.sigSubs[roomID] = map[string]chan SignalEnvelope{}
	}
	if old, ok := b.sigSubs[roomID][key]; ok {
		close(old)
	}
	b.sigSubs[roomID][key] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if m := b.sigSubs[roomID]; m != nil {
			if cur, ok := m[key]; ok && cur == ch {
				delete(m, key)
				if len(m) == 0 {
					delete(b.sigSubs, roomID)
				}
			}
		}
		b.mu.Unlock()
	}
}

// SendSignal routes an envelope to one participant. Returns true if a
// mailbox existed (regardless of buffer state).
func (b *Bus) SendSignal(roomID, toKey string, env SignalEnvelope) bool {
	b.mu.Lock()
	ch, ok := b.sigSubs[roomID][toKey]
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- env:
	default:
		// drop: WebRTC will retry ICE; the renegotiation flow tolerates loss.
	}
	return true
}
