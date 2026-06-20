package rooms

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// staleAfter is how long a member can go without a heartbeat ping before
// the janitor evicts them. Browsers throttle background-tab setInterval,
// transient HTTPS blips swallow individual pings, and any eviction kills
// the room's WebRTC signal routes for that key — so we keep it generous
// and rely on EnsureMember to re-admit on the next user action.
const staleAfter = 60 * time.Second

// liveRoom holds the in-memory live state for one room.
type liveRoom struct {
	Admin    string // participant key of current admin (Identity.Key())
	Members  map[string]Identity
	Pending  map[string]Identity // key -> identity awaiting approval
	LastSeen map[string]time.Time
}

// State is the in-memory presence + approval state for all rooms.
// One process. Lost on restart — that's intentional (live conference state).
type State struct {
	mu       sync.Mutex
	rooms    map[string]*liveRoom
	onChange func(roomID string)
	onEmpty  func(roomID string)
}

func NewState() *State {
	return &State{rooms: map[string]*liveRoom{}}
}

func (s *State) get(roomID string) *liveRoom {
	r, ok := s.rooms[roomID]
	if !ok {
		r = &liveRoom{
			Members:  map[string]Identity{},
			Pending:  map[string]Identity{},
			LastSeen: map[string]time.Time{},
		}
		s.rooms[roomID] = r
	}
	return r
}

// Touch records that key is still alive at `now`. Called from the heartbeat
// endpoint. No-op if key isn't a current member.
func (s *State) Touch(roomID, key string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(roomID)
	if _, ok := r.Members[key]; !ok {
		return
	}
	r.LastSeen[key] = now
}

// SweepStale evicts members whose last-seen ping is older than staleAfter.
// Returns the rooms whose presence changed and, separately, the rooms that
// became empty as a result (no members and no pending). Emptied rooms are
// NOT included in `changed` — the caller resets them, which publishes its
// own presence event, so a duplicate would be redundant.
func (s *State) SweepStale(now time.Time) (changed, emptied []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for roomID, r := range s.rooms {
		dirty := false
		for key, id := range r.Members {
			last, ok := r.LastSeen[key]
			if !ok {
				continue
			}
			if now.Sub(last) <= staleAfter {
				continue
			}
			delete(r.Members, key)
			delete(r.LastSeen, key)
			dirty = true
			// Auto-transfer admin if the stale member held that slot.
			if r.Admin == key {
				r.Admin = ""
				var oldest *Identity
				for _, m := range r.Members {
					if m.IsGuest() {
						continue
					}
					if oldest == nil || m.JoinedAt.Before(oldest.JoinedAt) {
						cp := m
						oldest = &cp
					}
				}
				if oldest != nil {
					r.Admin = oldest.Key()
				}
			}
			_ = id // satisfy linter
		}
		if !dirty {
			continue
		}
		if len(r.Members) == 0 && len(r.Pending) == 0 {
			// Last live participant evicted — tear down so a later Snapshot
			// returns clean state, and flag for the empty-room reset.
			r.Admin = ""
			emptied = append(emptied, roomID)
		} else {
			changed = append(changed, roomID)
		}
	}
	return changed, emptied
}

// RunJanitor periodically sweeps stale members. Stops when ctx is done.
// Publishes a presence event on every room that changed, so any open SSE
// stream resyncs the participant list.
func (s *State) RunJanitor(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, emptied := s.SweepStale(time.Now().UTC())
			for _, rid := range changed {
				if log != nil {
					log.Info("rooms janitor evicted stale members", "room", rid)
				}
				s.janitorNotify(rid)
			}
			for _, rid := range emptied {
				if log != nil {
					log.Info("rooms janitor reset emptied room", "room", rid)
				}
				s.emptyNotify(rid)
			}
		}
	}
}

// janitorNotify is set by NewService so the janitor can fire presence
// events without importing the bus directly. Keeps the cycle clean.
func (s *State) janitorNotify(roomID string) {
	if s.onChange != nil {
		s.onChange(roomID)
	}
}

// OnChange installs the callback the janitor calls after evicting stale
// members. Service wires Bus.PublishRoom(presence) here.
func (s *State) OnChange(fn func(roomID string)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// emptyNotify fires the onEmpty callback for a room the janitor just emptied.
// Read under the lock so it can't race OnEmpty installation at boot.
func (s *State) emptyNotify(roomID string) {
	s.mu.Lock()
	fn := s.onEmpty
	s.mu.Unlock()
	if fn != nil {
		fn(roomID)
	}
}

// OnEmpty installs the callback the janitor calls when it evicts the last
// participant from a room. Service wires resetRoom here so a room whose tab
// just closed without a clean /leave still returns to default state.
func (s *State) OnEmpty(fn func(roomID string)) {
	s.mu.Lock()
	s.onEmpty = fn
	s.mu.Unlock()
}

// Snapshot returns a copy of one room's live state.
type Snapshot struct {
	AdminKey     string
	Members      []Identity
	Pending      []Identity
	MemberCount  int
	PendingCount int
}

func (s *State) Snapshot(roomID string) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(roomID)
	snap := Snapshot{
		AdminKey:     r.Admin,
		MemberCount:  len(r.Members),
		PendingCount: len(r.Pending),
		Members:      make([]Identity, 0, len(r.Members)),
		Pending:      make([]Identity, 0, len(r.Pending)),
	}
	for _, m := range r.Members {
		snap.Members = append(snap.Members, m)
	}
	for _, p := range r.Pending {
		snap.Pending = append(snap.Pending, p)
	}
	sort.Slice(snap.Members, func(i, j int) bool {
		return snap.Members[i].JoinedAt.Before(snap.Members[j].JoinedAt)
	})
	sort.Slice(snap.Pending, func(i, j int) bool {
		return snap.Pending[i].JoinedAt.Before(snap.Pending[j].JoinedAt)
	})
	return snap
}

// AdminKey returns the current admin's participant key (empty if none).
func (s *State) AdminKey(roomID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(roomID).Admin
}

// MemberCount returns the count without copying the member slice.
func (s *State) MemberCount(roomID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.get(roomID).Members)
}

// JoinResult describes what happened in a Join attempt.
type JoinResult struct {
	Admitted   bool   // true = added to Members
	Pending    bool   // true = added to Pending
	BecameAdmin bool  // true = this caller is now admin
	Reason     string // when neither: "full" | "already_member" | "no_admin_yet"
}

// Join applies room policy and updates live state. `isPublic` is read from
// the persisted room. `viaInvite` is true when the caller arrived through a
// valid invite token (admits without approval, like a public room).
func (s *State) Join(roomID string, id Identity, isPublic, viaInvite bool, now time.Time) JoinResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(roomID)
	key := id.Key()

	if _, ok := r.Members[key]; ok {
		// Already in — refresh identity (name might've changed) and return.
		id.JoinedAt = r.Members[key].JoinedAt
		r.Members[key] = id
		became := r.Admin == key
		return JoinResult{Admitted: true, BecameAdmin: became, Reason: "already_member"}
	}
	delete(r.Pending, key) // clear any stale pending entry

	if len(r.Members) >= MaxParticipants {
		return JoinResult{Reason: "full"}
	}
	id.JoinedAt = now

	// First joiner becomes admin (auth users only — guests can never be admin).
	if r.Admin == "" && !id.IsGuest() {
		r.Admin = key
		r.Members[key] = id
		r.LastSeen[key] = now
		return JoinResult{Admitted: true, BecameAdmin: true}
	}

	// Public or invite-arrived → straight in.
	if isPublic || viaInvite {
		r.Members[key] = id
		r.LastSeen[key] = now
		return JoinResult{Admitted: true}
	}

	// No admin yet, guest arriving without invite: refuse — there's no one
	// to approve them. (This path is rare in practice; UI gates it earlier.)
	if r.Admin == "" {
		return JoinResult{Reason: "no_admin_yet"}
	}

	// Private room: queue for approval.
	r.Pending[key] = id
	return JoinResult{Pending: true}
}

// Approve moves a pending caller into Members. Only admin should invoke.
// Returns the promoted Identity (zero if not pending).
func (s *State) Approve(roomID, byKey, targetKey string) (Identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(roomID)
	if r.Admin != byKey {
		return Identity{}, false
	}
	id, ok := r.Pending[targetKey]
	if !ok {
		return Identity{}, false
	}
	if len(r.Members) >= MaxParticipants {
		return Identity{}, false
	}
	delete(r.Pending, targetKey)
	r.Members[targetKey] = id
	r.LastSeen[targetKey] = time.Now().UTC()
	return id, true
}

// Decline drops a pending caller. Admin only.
func (s *State) Decline(roomID, byKey, targetKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(roomID)
	if r.Admin != byKey {
		return false
	}
	if _, ok := r.Pending[targetKey]; !ok {
		return false
	}
	delete(r.Pending, targetKey)
	return true
}

// Leave removes a participant. Returns (newAdminKey, roomEmpty) — newAdminKey
// is set when admin role moved.
func (s *State) Leave(roomID, key string) (newAdmin string, empty bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(roomID)
	delete(r.Members, key)
	delete(r.Pending, key)
	delete(r.LastSeen, key)

	if r.Admin == key {
		// Pick oldest remaining auth-user member as new admin.
		var oldest *Identity
		for _, m := range r.Members {
			if m.IsGuest() {
				continue
			}
			if oldest == nil || m.JoinedAt.Before(oldest.JoinedAt) {
				cp := m
				oldest = &cp
			}
		}
		if oldest != nil {
			r.Admin = oldest.Key()
			newAdmin = r.Admin
		} else {
			r.Admin = ""
		}
	}
	if len(r.Members) == 0 && len(r.Pending) == 0 {
		// Tear the live record down so subsequent Snapshot returns clean state.
		r.Admin = ""
		empty = true
	}
	return newAdmin, empty
}

// Promote transfers admin to another current member. Caller must be admin.
func (s *State) Promote(roomID, byKey, targetKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.get(roomID)
	if r.Admin != byKey {
		return false
	}
	target, ok := r.Members[targetKey]
	if !ok || target.IsGuest() {
		return false
	}
	r.Admin = targetKey
	return true
}

// IsMember reports whether key is currently in the room.
func (s *State) IsMember(roomID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.get(roomID).Members[key]
	return ok
}

// IsAdmin reports whether key currently holds the admin slot.
func (s *State) IsAdmin(roomID, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(roomID).Admin == key
}
