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

// Project-todo status constants. `done` mirrors the legacy `done`
// column (kept in sync on every write) so index-card counts and the
// checkbox toggle stay valid.
const (
	TodoStatusTodo       = "todo"
	TodoStatusInProgress = "in_progress"
	TodoStatusDone       = "done"
)

// TodoStatuses is the canonical order shown in the per-row status select.
var TodoStatuses = []string{TodoStatusTodo, TodoStatusInProgress, TodoStatusDone}

// ValidTodoStatus reports whether s is one of the known statuses.
func ValidTodoStatus(s string) bool {
	return s == TodoStatusTodo || s == TodoStatusInProgress || s == TodoStatusDone
}

// Todo is one row inside a project's checklist. Beyond the original
// body+done checkbox it carries an agile-ish status, a completion stamp,
// and an optional assignee (with a display-name snapshot for rendering).
type Todo struct {
	ID             string
	ProjectID      string
	Body           string
	Done           bool
	Status         string
	SortOrder      int
	CreatedBy      string
	AssigneeUserID string
	AssigneeName   string // snapshot from the membership roster; "" when unassigned
	CompletedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Attachment links an uploads row to a project. Files themselves live
// in internal/uploads — this table only carries the per-project metadata
// (display filename, who uploaded, when, what bucket).
type Attachment struct {
	ID         string
	ProjectID  string
	UploadID   string
	Filename   string
	MIME       string
	SizeBytes  int64
	UploaderID string
	Category   string // free text; UI suggests common / api_docs / design / other
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
