// Package projects implements the per-community Projects feature: a
// collaborative page with title, description, project-local todos,
// drag-drop document attachments, and an inline comment thread. Feature
// gating happens at route-mount time via config.ProjectsEnabled.
package projects

import "time"

// Project is the persistent row plus a few derived counts used by the
// index grid.
type Project struct {
	ID              string
	CommunityID     string
	CreatorUserID   string
	Title           string
	DescriptionMD   string
	DescriptionHTML string
	ArchivedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// IsArchived reports whether the project is currently archived.
func (p Project) IsArchived() bool { return p.ArchivedAt != nil }

// Todo is one row inside a project's checklist.
type Todo struct {
	ID         string
	ProjectID  string
	Body       string
	Done       bool
	SortOrder  int
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Attachment links an uploads row to a project. Files themselves live
// in internal/uploads — this table only carries the per-project metadata
// (display filename, who uploaded, when).
type Attachment struct {
	ID         string
	ProjectID  string
	UploadID   string
	Filename   string
	MIME       string
	SizeBytes  int64
	UploaderID string
	CreatedAt  time.Time
}

// Comment is one inline discussion entry. Edit/delete follow the same
// edit-grace window as forum posts.
type Comment struct {
	ID        string
	ProjectID string
	AuthorID  string
	BodyMD    string
	BodyHTML  string
	EditedAt  *time.Time
	DeletedAt *time.Time
	CreatedAt time.Time
}

// IsDeleted reports a soft-delete tombstone.
func (c Comment) IsDeleted() bool { return c.DeletedAt != nil }

// Event is the per-project SSE fan-out payload. Subscribers re-render
// the affected fragment when one of these arrives.
type Event struct {
	Kind string // header | todos | attachments | comments | archive
}

// IndexRow is the lightweight aggregate used to render one card on the
// index grid. Counts are computed via SQL aggregates so the page renders
// without a per-project N+1.
type IndexRow struct {
	Project
	TodoTotal       int
	TodoDone        int
	AttachmentCount int
	CommentCount    int
}
