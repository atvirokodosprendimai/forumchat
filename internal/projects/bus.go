package projects

import "sync"

// Bus fans Event values out to every subscriber of one project's SSE
// stream. Same shape as rooms.Bus (per-room) and forum.Bus (per-thread).
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan Event]struct{} // projectID -> set
}

// NewBus is the zero-state constructor.
func NewBus() *Bus {
	return &Bus{subs: map[string]map[chan Event]struct{}{}}
}

// SubscribeProject registers a channel for one project's events. The
// returned unsubscribe func must be called when the stream closes.
func (b *Bus) SubscribeProject(projectID string) (<-chan Event, func()) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	set := b.subs[projectID]
	if set == nil {
		set = map[chan Event]struct{}{}
		b.subs[projectID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if s := b.subs[projectID]; s != nil {
			delete(s, ch)
			if len(s) == 0 {
				delete(b.subs, projectID)
			}
		}
		b.mu.Unlock()
	}
}

// PublishProject delivers ev to every current subscriber without
// blocking. Slow consumers drop events — the next interaction triggers
// a fresh re-render anyway.
func (b *Bus) PublishProject(projectID string, ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[projectID] {
		select {
		case ch <- ev:
		default:
		}
	}
}
