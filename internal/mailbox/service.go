package mailbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/projects"
)

// MaterialiseInput captures the move-attachment request from the inbox
// UI. The viewer must be admin in the ingest's community AND the
// chosen project must belong to that community.
type MaterialiseInput struct {
	AttachmentID string
	ProjectID    string
	Category     string
	MoverID      string // user_id (admin user clicking "Move")
}

// MaterialiseResult is returned to the caller for SSE re-render.
type MaterialiseResult struct {
	CommunityID    string
	IngestID       string
	BecameConsumed bool
}

// Service orchestrates the cross-package writes that aren't pure repo
// operations: fetching IMAP bytes, calling projects.Service to wrap
// the upload + project_attachments row, stamping mailbox cursor
// state, and auto-creating issues from to_issue=true filters.
type Service struct {
	Repo         *Repo
	Cfg          AccountConfig
	Projects     *projects.Service
	Projs        *projects.Repo
	AuthRepo     *auth.Repo
	SystemUserID string // optional env override

	// inboxProjectCache memoises the per-community "Inbox" project id so
	// the auto-issue path doesn't re-query on every match.
	inboxMu    sync.Mutex
	inboxCache map[string]string
}

// NewService wires the dependencies.
func NewService(repo *Repo, cfg AccountConfig, ps *projects.Service, pr *projects.Repo, ar *auth.Repo, systemUserID string) *Service {
	return &Service{
		Repo:         repo,
		Cfg:          cfg,
		Projects:     ps,
		Projs:        pr,
		AuthRepo:     ar,
		SystemUserID: systemUserID,
		inboxCache:   map[string]string{},
	}
}

// Materialise fetches a single MIME part from the IMAP server via
// BODY.PEEK[...] (never \Seen-marking the source message), saves it
// through uploads.Store, creates a project_attachments row, and
// stamps the mailbox attachment with the resulting upload_id +
// target project + category. When all of the parent ingest's
// attachments are moved, the ingest's status flips to consumed.
func (s *Service) Materialise(ctx context.Context, in MaterialiseInput) (MaterialiseResult, error) {
	if s.Projects == nil || s.Projs == nil {
		return MaterialiseResult{}, errors.New("mailbox: materialise needs projects service")
	}
	look, err := s.Repo.AttachmentByID(ctx, in.AttachmentID)
	if err != nil {
		return MaterialiseResult{}, fmt.Errorf("attachment lookup: %w", err)
	}
	if look.Attachment.UploadID != "" {
		return MaterialiseResult{}, errors.New("mailbox: attachment already materialised")
	}
	proj, err := s.Projs.ByID(ctx, in.ProjectID)
	if err != nil {
		return MaterialiseResult{}, fmt.Errorf("project lookup: %w", err)
	}
	// Matched ingest must move into its own community. Unassigned
	// ingests adopt the chosen project's community on materialise.
	if look.Ingest.CommunityID != "" && proj.CommunityID != look.Ingest.CommunityID {
		return MaterialiseResult{}, errors.New("mailbox: project belongs to a different community")
	}

	bytesData, err := s.fetchPart(look)
	if err != nil {
		return MaterialiseResult{}, fmt.Errorf("imap fetch part: %w", err)
	}

	att, err := s.Projects.AddAttachment(ctx,
		proj.ID,
		proj.CommunityID,
		in.MoverID,
		look.Attachment.MIME,
		look.Attachment.Filename,
		in.Category,
		bytes.NewReader(bytesData),
	)
	if err != nil {
		return MaterialiseResult{}, fmt.Errorf("project attachment: %w", err)
	}

	if err := s.Repo.MarkAttachmentMoved(ctx, in.AttachmentID, att.UploadID, proj.ID, in.Category); err != nil {
		return MaterialiseResult{}, fmt.Errorf("mark moved: %w", err)
	}

	// Unassigned ingest just got a community-of-record — record it so
	// the row leaves Unassigned and lands in the project's community.
	if look.Ingest.CommunityID == "" {
		if err := s.Repo.AssignIngestCommunity(ctx, look.Attachment.IngestID, proj.CommunityID); err != nil {
			return MaterialiseResult{}, fmt.Errorf("assign ingest community: %w", err)
		}
	}

	became, err := s.Repo.MarkIngestConsumedIfAllMoved(ctx, look.Attachment.IngestID)
	if err != nil {
		return MaterialiseResult{}, fmt.Errorf("consumed check: %w", err)
	}

	return MaterialiseResult{
		CommunityID:    proj.CommunityID,
		IngestID:       look.Attachment.IngestID,
		BecameConsumed: became,
	}, nil
}

// fetchPart opens a short-lived IMAP session, EXAMINEs the folder the
// attachment lives in, and pulls just the one MIME part identified by
// mime_part_id. No connection pooling in v1 — the click-to-fetch path
// is interactive and a fresh login per click is acceptable.
func (s *Service) fetchPart(look AttachmentLookup) ([]byte, error) {
	c, err := dial(s.Cfg)
	if err != nil {
		return nil, err
	}
	defer c.close()
	if _, err := c.examineReadOnly(look.FolderName); err != nil {
		return nil, err
	}
	return c.fetchPart(look.Ingest.UID, look.Attachment.MIMEPartID)
}

// AutoCreateIssueInput captures the work AutoCreateIssue needs.
// IngestID + CommunityID + Subject are persisted; TextBody and HTMLBody
// come from a Phase 7 IMAP refetch (the poll loop already has them via
// fetchEnvelopeWithBody).
type AutoCreateIssueInput struct {
	IngestID    string
	CommunityID string
	Subject     string
	TextBody    string
	HTMLBody    string
}

// AutoCreateIssue creates a project_issue from a matched email when
// the filter has to_issue=true. Idempotent: a second call with the
// same IngestID is a no-op once the link row exists.
//
// Resolution rules:
//   - Author = MAILBOX_SYSTEM_USER_ID if set, otherwise the oldest
//     admin in the target community. Matches the user's directive
//     "automatic chooses global admin if not preset".
//   - Project = a per-community "Inbox" project, lazily created on
//     first hit. Memoised on the service so subsequent hits are free.
//
// Returns the resulting issue ID (newly created or already linked).
func (s *Service) AutoCreateIssue(ctx context.Context, in AutoCreateIssueInput) (string, error) {
	if s.Projects == nil {
		return "", errors.New("mailbox: auto-issue requires projects service")
	}
	if has, err := s.Repo.HasIngestIssue(ctx, in.IngestID); err != nil {
		return "", err
	} else if has {
		return "", nil // already created — nothing to do
	}

	creatorID, err := s.resolveCreator(ctx, in.CommunityID)
	if err != nil {
		return "", fmt.Errorf("resolve creator: %w", err)
	}
	projectID, err := s.ensureInboxProject(ctx, in.CommunityID, creatorID)
	if err != nil {
		return "", fmt.Errorf("ensure inbox project: %w", err)
	}

	title := strings.TrimSpace(in.Subject)
	if title == "" {
		title = "(no subject)"
	}
	body := ExtractIssueBody(in.TextBody, in.HTMLBody)

	issue, err := s.Projects.CreateIssue(ctx, projectID, title, body, projects.Identity{
		UserID: creatorID,
		Name:   "Mailbox",
	})
	if err != nil {
		return "", fmt.Errorf("create issue: %w", err)
	}
	if err := s.Repo.InsertIngestIssue(ctx, in.IngestID, issue.ID); err != nil {
		return "", fmt.Errorf("link ingest issue: %w", err)
	}
	return issue.ID, nil
}

// resolveCreator implements the spec / plan rule: env override beats
// the auto-fallback to the oldest community admin.
func (s *Service) resolveCreator(ctx context.Context, communityID string) (string, error) {
	if s.SystemUserID != "" {
		return s.SystemUserID, nil
	}
	if s.AuthRepo == nil {
		return "", errors.New("mailbox: no system user and no auth repo to fall back to")
	}
	id, err := s.AuthRepo.OldestCommunityAdminID(ctx, communityID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("mailbox: no admin in community %s — cannot auto-create issue", communityID)
		}
		return "", err
	}
	return id, nil
}

// ensureInboxProject finds (or creates) a community-scoped project
// titled "Inbox" the auto-issues attach to. Cached by community id.
func (s *Service) ensureInboxProject(ctx context.Context, communityID, creatorID string) (string, error) {
	s.inboxMu.Lock()
	if pid, ok := s.inboxCache[communityID]; ok {
		s.inboxMu.Unlock()
		return pid, nil
	}
	s.inboxMu.Unlock()

	rows, err := s.Projs.ListActiveForCommunity(ctx, communityID)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if strings.EqualFold(r.Title, "Inbox") {
			s.inboxMu.Lock()
			s.inboxCache[communityID] = r.ID
			s.inboxMu.Unlock()
			return r.ID, nil
		}
	}

	p, err := s.Projects.CreateProject(ctx, communityID, creatorID, "Inbox",
		"Auto-generated container for emails that filters with `to_issue=true` turn into issues.")
	if err != nil {
		return "", err
	}
	s.inboxMu.Lock()
	s.inboxCache[communityID] = p.ID
	s.inboxMu.Unlock()
	return p.ID, nil
}
