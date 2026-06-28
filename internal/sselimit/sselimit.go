// Package sselimit caps the number of concurrent Server-Sent-Events streams a
// single user may hold open at once (FIX1 M5). Every realtime page opens a
// long-lived SSE connection; without a cap a scripted client could open
// thousands and exhaust goroutines / DB poll loops cheaply. In-process only,
// consistent with the chat Bus and presence tracker — each node enforces its
// own cap.
package sselimit

import "sync"

// Limiter tracks live streams per user key. The zero value is unusable; call New.
type Limiter struct {
	mu  sync.Mutex
	n   map[string]int
	max int
}

// New builds a Limiter allowing up to max concurrent streams per user. max <= 0
// disables the cap (Acquire always succeeds).
func New(max int) *Limiter {
	return &Limiter{n: make(map[string]int), max: max}
}

// Acquire reserves a stream slot for userID. It returns a release func (always
// safe to call, including via defer) and ok=false when the user is already at
// the cap. An empty userID (anonymous / token-scoped guest) or a disabled
// limiter is never capped — those streams are bounded by other means (invite
// tokens, signed URLs).
func (l *Limiter) Acquire(userID string) (release func(), ok bool) {
	if l == nil || l.max <= 0 || userID == "" {
		return func() {}, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n[userID] >= l.max {
		return func() {}, false
	}
	l.n[userID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.n[userID] <= 1 {
				delete(l.n, userID)
			} else {
				l.n[userID]--
			}
		})
	}, true
}
