// Package mailbox implements the read-only IMAP ingest pipeline that
// feeds /inbox. A single shared mailbox is polled; per-community From:
// filters route matched messages into email_ingest rows visible to
// community admins in the global inbox UI.
package mailbox

import "time"

// Account is the singleton row in mailbox_account describing the IMAP
// endpoint. Populated at boot from env via EnsureAccount.
type Account struct {
	ID         string
	Host       string
	Port       int
	Username   string
	Password   string
	TLSMode    string // tls | starttls | none
	LastPollAt *time.Time
	LastError  string
	CreatedAt  time.Time
}

// Folder holds the per-folder sync cursor. UIDVALIDITY drift triggers a
// full re-scan of that folder; other folders are unaffected.
type Folder struct {
	ID          string
	AccountID   string
	Name        string
	UIDValidity uint32
	LastUID     uint32
	Enabled     bool
	LastSeenAt  *time.Time
	LastError   string
}

// FilterKind is the discriminator between exact-address and
// wildcard-domain filters.
type FilterKind string

const (
	FilterKindAddress FilterKind = "address"
	FilterKindDomain  FilterKind = "domain"
)

// Filter is one community routing rule. Patterns are stored lowercase
// with the leading '@' for domain kind.
type Filter struct {
	ID          string
	CommunityID string
	Kind        FilterKind
	Pattern     string
	ToIssue     bool
	CreatedBy   string
	CreatedAt   time.Time
}

// IngestStatus is the lifecycle marker on an email_ingest row.
type IngestStatus string

const (
	IngestQueued    IngestStatus = "queued"
	IngestConsumed  IngestStatus = "consumed"
	IngestDiscarded IngestStatus = "discarded"
)

// Ingest is one matched message persisted at poll time. Bytes of body
// and attachments are NOT in this row — only envelope + routing.
type Ingest struct {
	ID              string
	FolderID        string
	UID             uint32
	UIDValidity     uint32
	MessageID       string
	FromAddr        string
	FromName        string
	Subject         string
	ReceivedAt      time.Time
	CommunityID     string
	Status          IngestStatus
	MatchedFilterID string
	CreatedAt       time.Time
}

// Attachment is metadata-only at poll time. UploadID is populated when
// a user materialises the attachment into a project_attachments row.
type Attachment struct {
	ID               string
	IngestID         string
	Filename         string
	MIME             string
	SizeBytes        int64
	MIMEPartID       string // e.g. "2", "2.1" — used in BODY.PEEK[2.1]
	UploadID         string
	MovedToProjectID string
	MovedCategory    string
	MovedAt          *time.Time
	CreatedAt        time.Time
}

// QueueCursor is the opaque pagination token for /inbox infinite scroll.
// Encodes (received_at_unix_ms, id) so ties at the same millisecond
// resolve deterministically.
type QueueCursor struct {
	ReceivedAtUnixMS int64
	ID               string
}

// UnassignedCommunityID is the sentinel value passed in
// QueueQuery.CommunityFilter to view the unfiltered (NULL community_id)
// pile. Picked to be invalid as a real uuid so it can't collide.
const UnassignedCommunityID = "_unassigned"

// QueueQuery is the read-model input for /inbox and /inbox/more.
type QueueQuery struct {
	// AdminCommunityIDs is the viewer's admin/mod community set. The
	// caller is responsible for populating this from auth.Repo so the
	// repo only handles SQL.
	AdminCommunityIDs []string
	// CommunityFilter, when non-empty, narrows to one community. The
	// sentinel UnassignedCommunityID narrows to NULL community_id rows.
	// Empty string = union of admin communities + unassigned.
	CommunityFilter string
	// HasAttachments narrows to rows with at least one
	// email_ingest_attachment. Combines additively with the search +
	// pill filters.
	HasAttachments bool
	// Cursor is the opaque pagination token. Nil for the first page.
	Cursor *QueueCursor
	// Limit caps the page size. Defaults applied in the handler.
	Limit int
}
