package privatemsg

import "time"

type Status string

const (
	StatusPending  Status = "pending"
	StatusAccepted Status = "accepted"
	StatusDeclined Status = "declined"
)

type Thread struct {
	ID                  string
	InitiatorUserID     string
	RecipientUserID     string
	Status              Status
	SourceCommunityID   string // empty when not started from a community chat
	SourceChatMessageID string
	LastMessageAt       time.Time
	CreatedAt           time.Time
}

// Other returns the user-id on the opposite side of the thread from viewer.
func (t Thread) Other(viewer string) string {
	if t.InitiatorUserID == viewer {
		return t.RecipientUserID
	}
	return t.InitiatorUserID
}

// HasMember returns true if userID is one of the two participants.
func (t Thread) HasMember(userID string) bool {
	return t.InitiatorUserID == userID || t.RecipientUserID == userID
}

type Message struct {
	ID           string
	ThreadID     string
	AuthorUserID string
	Body         string
	BodyHTML     string
	CreatedAt    time.Time
}

// InboxRow is a Thread joined with the other party's display name + the
// latest message snippet — what the inbox list renders.
type InboxRow struct {
	Thread         Thread
	OtherUserID    string
	OtherUserName  string
	LastSnippet    string
	UnreadCount    int
	IsIncoming     bool // viewer is the recipient (matters for pending: only recipient can accept/decline)
}
