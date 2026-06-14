package rooms

import "time"

// MaxParticipants is the hard cap per room. The mesh signaling stays
// usable; the client downgrades to audio-only past VideoCap.
const (
	MaxParticipants = 16
	VideoCap        = 8
	NumRooms        = 8
)

// Room is the persistent slot record. Live presence lives in State.
type Room struct {
	ID          string
	CommunityID string
	Slot        int
	Name        string
	IsPublic    bool
	AdminUserID string // empty when no admin / room idle
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ChatMessage is a persisted room-chat row.
type ChatMessage struct {
	ID           string
	RoomID       string
	CommunityID  string
	AuthorUserID string // empty for guest
	AuthorName   string
	Body         string
	BodyHTML     string
	CreatedAt    time.Time
}

// Invite is a long shareable token that lets external (logged-out) guests
// join one specific room.
type Invite struct {
	Token     string
	RoomID    string
	CreatedBy string
	CreatedAt time.Time
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

func (i Invite) Active(now time.Time) bool {
	if i.RevokedAt != nil {
		return false
	}
	if i.ExpiresAt != nil && now.After(*i.ExpiresAt) {
		return false
	}
	return true
}

// Identity is who a connection belongs to: either an auth user or an
// ephemeral guest joined via invite token.
type Identity struct {
	UserID    string // populated for auth users
	GuestID   string // populated for invite-guests
	Name      string // display name shown in the room
	JoinedAt  time.Time
}

// IsGuest reports whether this identity is an invite-link guest.
func (id Identity) IsGuest() bool { return id.UserID == "" && id.GuestID != "" }

// Key returns a stable participant key suitable for map indexing.
// Auth users key off UserID; guests off "guest:" + GuestID.
func (id Identity) Key() string {
	if id.UserID != "" {
		return "u:" + id.UserID
	}
	return "g:" + id.GuestID
}
