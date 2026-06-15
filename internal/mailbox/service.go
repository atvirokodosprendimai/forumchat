package mailbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"

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
// the upload + project_attachments row, and stamping mailbox cursor
// state.
type Service struct {
	Repo     *Repo
	Cfg      AccountConfig
	Projects *projects.Service
	Projs    *projects.Repo
}

// NewService wires the dependencies.
func NewService(repo *Repo, cfg AccountConfig, ps *projects.Service, pr *projects.Repo) *Service {
	return &Service{Repo: repo, Cfg: cfg, Projects: ps, Projs: pr}
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
	if proj.CommunityID != look.Ingest.CommunityID {
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
