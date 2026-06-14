package projects

import (
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

// Identity is the unified caller identity used by the issue + comment
// + attachment code paths. Either UserID (auth member) or GuestID
// (share-link guest) is populated, never both. Name is the display-
// name snapshot persisted on creation rows so guests show up sensibly
// after their session expires.
type Identity struct {
	UserID  string
	GuestID string
	Name    string
	Role    auth.Role // populated for auth users; RoleMember default for guests
}

// IsGuest reports whether this is a share-link guest.
func (id Identity) IsGuest() bool { return id.UserID == "" && id.GuestID != "" }

// Key returns a stable participant key for map indexing.
func (id Identity) Key() string {
	if id.UserID != "" {
		return "u:" + id.UserID
	}
	return "g:" + id.GuestID
}

// Issue status constants; CHECK in the migration enforces the same set.
const (
	IssueOpen       = "open"
	IssueTriaged    = "triaged"
	IssueInProgress = "in_progress"
	IssueClosed     = "closed"
)

// IssueStatuses is the canonical order shown in the status dropdown.
var IssueStatuses = []string{IssueOpen, IssueTriaged, IssueInProgress, IssueClosed}

// Issue is one ticket attached to a project.
type Issue struct {
	ID              string
	ProjectID       string
	Title           string
	BodyMD          string
	BodyHTML        string
	Status          string
	CreatorUserID   string // empty for guest
	CreatorGuestID  string // empty for auth user
	CreatorName     string // display-name snapshot
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IsGuestAuthored reports whether the creator was a share-link guest.
func (i Issue) IsGuestAuthored() bool { return i.CreatorUserID == "" && i.CreatorGuestID != "" }

// IsClosed reports whether the issue is in the closed state.
func (i Issue) IsClosed() bool { return i.Status == IssueClosed }

// IssueComment is one entry in an issue thread.
type IssueComment struct {
	ID             string
	IssueID        string
	AuthorUserID   string
	AuthorGuestID  string
	AuthorName     string
	BodyMD         string
	BodyHTML       string
	EditedAt       *time.Time
	DeletedAt      *time.Time
	CreatedAt      time.Time
}

// IsDeleted reports a soft-delete tombstone.
func (c IssueComment) IsDeleted() bool { return c.DeletedAt != nil }

// IssueAttachment links an uploads row to an issue (or one of its
// comments). For now only image MIMEs are accepted via the existing
// uploads.Store.Save whitelist.
type IssueAttachment struct {
	ID                string
	IssueID           string
	CommentID         string // empty when attached to the issue body itself
	UploadID          string
	UploaderUserID    string
	UploaderGuestID   string
	UploaderName      string
	CreatedAt         time.Time
}

// GuestInvite is one share-link record.
type GuestInvite struct {
	Token      string
	ProjectID  string
	CreatedBy  string
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// Active reports whether the invite is currently usable at `now`.
func (g GuestInvite) Active(now time.Time) bool {
	if g.RevokedAt != nil {
		return false
	}
	if g.ExpiresAt != nil && now.After(*g.ExpiresAt) {
		return false
	}
	return true
}

// IssueEvent is the SSE fan-out payload for a single issue's stream.
// Kinds: "issue" (title/body/status), "comments", "attachments".
type IssueEvent struct {
	Kind string
}

// IssueListEvent is the SSE fan-out payload for the project-level
// issues panel (so the issues list re-renders on the project page when
// new issues are created elsewhere).
type IssueListEvent struct{}
