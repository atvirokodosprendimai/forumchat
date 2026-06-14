package projects

import "time"

// DiscussionThread is one project-scoped forum thread.
type DiscussionThread struct {
	ID              string
	ProjectID       string
	Subject         string
	BodyMD          string
	BodyHTML        string
	CreatorUserID   string
	CreatorGuestID  string
	CreatorName     string
	DeletedAt       *time.Time
	LastActivityAt  time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IsGuestAuthored reports whether the creator was a share-link guest.
func (t DiscussionThread) IsGuestAuthored() bool {
	return t.CreatorUserID == "" && t.CreatorGuestID != ""
}

// IsDeleted reports a soft-delete tombstone.
func (t DiscussionThread) IsDeleted() bool { return t.DeletedAt != nil }

// DiscussionReply is one entry in a thread.
type DiscussionReply struct {
	ID             string
	ThreadID       string
	QuotedReplyID  string
	AuthorUserID   string
	AuthorGuestID  string
	AuthorName     string
	BodyMD         string
	BodyHTML       string
	EditedAt       *time.Time
	DeletedAt      *time.Time
	CreatedAt      time.Time
}

// IsGuestAuthored reports whether the reply author was a share-link guest.
func (r DiscussionReply) IsGuestAuthored() bool {
	return r.AuthorUserID == "" && r.AuthorGuestID != ""
}

// IsDeleted reports a soft-delete tombstone.
func (r DiscussionReply) IsDeleted() bool { return r.DeletedAt != nil }

// DiscussionThreadRow is the list-page payload — thread + reply count.
type DiscussionThreadRow struct {
	DiscussionThread
	ReplyCount int
}
