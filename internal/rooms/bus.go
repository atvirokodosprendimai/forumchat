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
	roomSubs map[string]map[chan Event]struct{}              // roomID -> set
	sigSubs  map[string]map[string]chan SignalEnvelope       // roomID -> key -> ch
	// sigQueue holds envelopes sent before the recipient's mailbox was
	// open. Drained the moment the peer subscribes. Without this, the
	// very first SDP offer between two browsers races their signal-stream
	// connect and gets dropped, leaving both tiles black until someone
	// triggers a renegotiation by adding a track.
	sigQueue map[string]map[string][]SignalEnvelope // roomID -> toKey -> envs
}

const sigQueueCap = 64

func NewBus() *Bus {
	return &Bus{
		roomSubs: map[string]map[chan Event]struct{}{},
		sigSubs:  map[string]map[string]chan SignalEnvelope{},
		sigQueue: map[string]map[string][]SignalEnvelope{},
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
	ch := make(chan SignalEnvelope, 64)
	b.mu.Lock()
	if b.sigSubs[roomID] == nil {
		b.sigSubs[roomID] = map[string]chan SignalEnvelope{}
	}
	if old, ok := b.sigSubs[roomID][key]; ok {
		close(old)
	}
	b.sigSubs[roomID][key] = ch
	// Drain anything that arrived before this subscription. Order is
	// preserved (slice append-order), which matters for SDP exchange.
	if q := b.sigQueue[roomID]; q != nil {
		for _, env := range q[key] {
			select {
			case ch <- env:
			default:
			}
		}
		delete(q, key)
		if len(q) == 0 {
			delete(b.sigQueue, roomID)
		}
	}
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

// SendSignal routes an envelope to one participant. If no mailbox is
// subscribed yet (peer's signal stream hasn't connected) the envelope is
// queued and flushed on the next SubscribeSignal for that key. Returns
// true if delivered immediately, false if queued or buffer-dropped.
func (b *Bus) SendSignal(roomID, toKey string, env SignalEnvelope) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch, ok := b.sigSubs[roomID][toKey]
	if !ok {
		if b.sigQueue[roomID] == nil {
			b.sigQueue[roomID] = map[string][]SignalEnvelope{}
		}
		if len(b.sigQueue[roomID][toKey]) < sigQueueCap {
			b.sigQueue[roomID][toKey] = append(b.sigQueue[roomID][toKey], env)
		}
		return false
	}
	select {
	case ch <- env:
	default:
		// drop: WebRTC will retry ICE; the renegotiation flow tolerates loss.
	}
	return true
}

// ClearSignalQueue drops any queued envelopes for a peer key. Called by
// service.Leave so a peer that re-joins later doesn't replay a stale SDP
// offer from a now-defunct RTCPeerConnection.
func (b *Bus) ClearSignalQueue(roomID, key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if q := b.sigQueue[roomID]; q != nil {
		delete(q, key)
		if len(q) == 0 {
			delete(b.sigQueue, roomID)
		}
	}
}
