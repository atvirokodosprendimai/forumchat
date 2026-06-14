package privatemsg

import "sync"

// Bus fans out per-user notifications in-process. Used to wake the
// SSE handlers (badge + thread streams) of every open tab the user has,
// after a write path persists a thread or message.
type Bus struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{} // userID -> set of channels
}

func NewBus() *Bus { return &Bus{subs: map[string]map[chan struct{}]struct{}{}} }

func (b *Bus) Subscribe(userID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	set := b.subs[userID]
	if set == nil {
		set = map[chan struct{}]struct{}{}
		b.subs[userID] = set
	}
	set[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if s := b.subs[userID]; s != nil {
			delete(s, ch)
			if len(s) == 0 {
				delete(b.subs, userID)
			}
		}
		b.mu.Unlock()
	}
}

// Notify pings every subscriber registered for userID. Non-blocking.
func (b *Bus) Notify(userID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs[userID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
