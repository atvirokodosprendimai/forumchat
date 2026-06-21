package mailbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/projects"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// InboxProjectSentinel is the prefix used in the per-community "Inbox
// (auto)" dropdown option. Format: "_inbox:<communityID>". Service.
// Materialise expands it via ensureInboxProject so a community without
// any real projects still has a valid Move target.
const InboxProjectSentinel = "_inbox"

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

	projectID := in.ProjectID
	if strings.HasPrefix(projectID, InboxProjectSentinel+":") {
		targetCID := strings.TrimPrefix(projectID, InboxProjectSentinel+":")
		if look.Ingest.CommunityID != "" && targetCID != look.Ingest.CommunityID {
			return MaterialiseResult{}, errors.New("mailbox: Inbox-auto sentinel points to a different community than the ingest")
		}
		creator, err := s.resolveCreator(ctx, targetCID)
		if err != nil {
			return MaterialiseResult{}, fmt.Errorf("resolve creator for Inbox project: %w", err)
		}
		pid, err := s.ensureInboxProject(ctx, targetCID, creator)
		if err != nil {
			return MaterialiseResult{}, fmt.Errorf("ensure Inbox project: %w", err)
		}
		projectID = pid
	}
	proj, err := s.Projs.ByID(ctx, projectID)
	if err != nil {
		return MaterialiseResult{}, fmt.Errorf("project lookup %q: %w", projectID, err)
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
	decoded := decodeAttachmentBytes(bytesData, look.Attachment.TransferEncoding)

	att, err := s.Projects.AddAttachment(ctx,
		proj.ID,
		proj.CommunityID,
		in.MoverID,
		look.Attachment.MIME,
		look.Attachment.Filename,
		in.Category,
		bytes.NewReader(decoded),
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

// AttachToIssue uploads a raw byte payload via projects.Service.
// AddIssueAttachment so the email's attachments land inline next to
// the auto-created issue. Caller provides decoded bytes (after
// decodeAttachmentBytes) and the original Content-Type + filename.
func (s *Service) AttachToIssue(ctx context.Context, issueID, communityID, mime, filename string, body []byte) (string, error) {
	if s.Projects == nil {
		return "", errors.New("mailbox: attach-to-issue requires projects service")
	}
	issue, err := s.Projs.IssueByID(ctx, issueID)
	if err != nil {
		return "", fmt.Errorf("issue lookup: %w", err)
	}
	creatorID, err := s.resolveCreator(ctx, communityID)
	if err != nil {
		return "", fmt.Errorf("resolve creator: %w", err)
	}
	att, err := s.Projects.AddIssueAttachment(ctx,
		issue.ProjectID, issueID, "", communityID, mime, filename,
		bytes.NewReader(body),
		projects.Identity{UserID: creatorID, Name: "Mailbox"},
	)
	if err != nil {
		return "", fmt.Errorf("add issue attachment: %w", err)
	}
	return att.UploadID, nil
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
// Returns the resulting issue ID (newly created OR empty when the
// ingest already had an issue from a prior run — the caller can skip
// side-effects like attaching files in that case).
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

// RefetchResult reports what the refetch pass changed.
type RefetchResult struct {
	BodyUpdated      bool
	AttachmentsAdded int
}

// RefetchIssueFromEmail re-runs the auto-issue pipeline (text decode,
// markdown render, attachment download) against the SOURCE email of
// the issue. Body is overwritten with freshly decoded text; attachments
// not yet present (SHA256 not in the existing project_issue_attachments
// set) are added. Used by the "Refetch from email" button on the issue
// page after the IMAP decode pipeline gets fixed.
func (s *Service) RefetchIssueFromEmail(ctx context.Context, issueID string) (RefetchResult, error) {
	var res RefetchResult
	if s.Projects == nil || s.Projs == nil {
		return res, errors.New("mailbox: refetch requires projects service")
	}
	ing, folder, _, err := s.Repo.IngestByIssueID(ctx, issueID)
	if err != nil {
		return res, fmt.Errorf("ingest lookup: %w", err)
	}
	issue, err := s.Projs.IssueByID(ctx, issueID)
	if err != nil {
		return res, fmt.Errorf("issue lookup: %w", err)
	}
	proj, err := s.Projs.ByID(ctx, issue.ProjectID)
	if err != nil {
		return res, fmt.Errorf("project lookup: %w", err)
	}

	c, err := dial(s.Cfg)
	if err != nil {
		return res, fmt.Errorf("imap dial: %w", err)
	}
	defer c.close()
	if _, err := c.examineReadOnly(folder); err != nil {
		return res, fmt.Errorf("examine %s: %w", folder, err)
	}
	env, text, html, err := c.fetchEnvelopeWithBody(ing.UID)
	if err != nil {
		return res, fmt.Errorf("fetch envelope+body: %w", err)
	}

	// Dedup against existing attachments by SHA256.
	existingSHA := map[string]string{}
	if existing, err := s.Projs.ListIssueAttachments(ctx, issueID); err == nil {
		for _, a := range existing {
			if u, err := s.Projects.Uploads.Get(ctx, a.UploadID); err == nil {
				existingSHA[u.SHA256] = a.UploadID
			}
		}
	}

	creatorID, err := s.resolveCreator(ctx, proj.CommunityID)
	if err != nil {
		return res, fmt.Errorf("resolve creator: %w", err)
	}

	// Save attachments first so the body rewriter has a destination
	// upload ID for every cid: reference. Build the cid -> upload map.
	cidToUpload := map[string]string{}
	for _, p := range env.Attachments {
		raw, err := c.fetchPartPath(ing.UID, parsePartPath(p.MIMEPartID))
		if err != nil {
			continue
		}
		decoded := decodeAttachmentBytes(raw, p.Encoding)
		sum := sha256.Sum256(decoded)
		hex := fmt.Sprintf("%x", sum[:])
		if uid, dup := existingSHA[hex]; dup {
			if p.ContentID != "" {
				cidToUpload[p.ContentID] = uid
			}
			continue
		}
		att, err := s.Projects.AddIssueAttachment(ctx,
			issue.ProjectID, issueID, "", proj.CommunityID, p.MIME, p.Filename,
			bytes.NewReader(decoded),
			projects.Identity{UserID: creatorID, Name: "Mailbox"},
		)
		if err != nil {
			continue
		}
		existingSHA[hex] = att.UploadID
		if p.ContentID != "" {
			cidToUpload[p.ContentID] = att.UploadID
		}
		res.AttachmentsAdded++
	}

	// Body refresh (with cid: rewrite if any inline parts).
	body := ExtractIssueBody(text, html)
	body = RewriteCIDImages(body, cidToUpload)
	bodyHTML, mdErr := render.RenderMarkdown(body)
	if mdErr != nil {
		return res, fmt.Errorf("render md: %w", mdErr)
	}
	if err := s.Projs.UpdateIssueBody(ctx, issueID, body, bodyHTML, time.Now().UTC()); err != nil {
		return res, fmt.Errorf("update issue body: %w", err)
	}
	res.BodyUpdated = true
	return res, nil
}

// RewriteIssueBodyCIDs reads the current issue body, rewrites every
// `cid:<contentID>` reference to point at the uploaded copy via the
// `upload://<uploadID>` placeholder scheme, re-renders the HTML, and
// persists. Used by the poll auto-issue path after attachments save.
func (s *Service) RewriteIssueBodyCIDs(ctx context.Context, issueID string, cidToUpload map[string]string) error {
	if len(cidToUpload) == 0 {
		return nil
	}
	issue, err := s.Projs.IssueByID(ctx, issueID)
	if err != nil {
		return err
	}
	newBody := RewriteCIDImages(issue.BodyMD, cidToUpload)
	if newBody == issue.BodyMD {
		return nil
	}
	html, err := render.RenderMarkdown(newBody)
	if err != nil {
		return err
	}
	return s.Projs.UpdateIssueBody(ctx, issueID, newBody, html, time.Now().UTC())
}

// ApplyFilterToPast retro-applies a filter to past email_ingest rows:
// idempotent backfill of unassigned rows that now match + AutoCreateIssue
// for every backfilled or previously-orphaned matched row when the filter
// has to_issue=true. Use when a new community+filter is created for a
// sender that already has historical mail sitting unassigned.
//
// Returns (matched, issuesCreated, error). matched counts newly tagged
// ingest rows (rows that already had this filter aren't recounted).
func (s *Service) ApplyFilterToPast(ctx context.Context, filterID string) (int64, int, error) {
	f, err := s.Repo.FilterByID(ctx, filterID)
	if err != nil {
		return 0, 0, fmt.Errorf("filter lookup: %w", err)
	}
	matched, err := s.Repo.BackfillIngestForFilter(ctx, f)
	if err != nil {
		return 0, 0, err
	}
	if !f.ToIssue {
		return matched, 0, nil
	}
	if s.Projects == nil {
		return matched, 0, errors.New("mailbox: apply-filter with to_issue requires projects service")
	}
	pendings, err := s.Repo.IngestsByFilter(ctx, filterID)
	if err != nil {
		return matched, 0, err
	}
	issued := 0
	for _, p := range pendings {
		if _, err := s.AutoCreateIssue(ctx, AutoCreateIssueInput{
			IngestID:    p.ID,
			CommunityID: p.CommunityID,
			Subject:     p.Subject,
			TextBody:    p.BodyText,
		}); err != nil {
			return matched, issued, fmt.Errorf("ingest %s: %w", p.ID, err)
		}
		issued++
	}
	return matched, issued, nil
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

	pid, err := s.Projects.EnsureNamedProject(ctx, communityID, creatorID, "Inbox",
		"Auto-generated container for emails that filters with `to_issue=true` turn into issues.")
	if err != nil {
		return "", err
	}
	s.inboxMu.Lock()
	s.inboxCache[communityID] = pid
	s.inboxMu.Unlock()
	return pid, nil
}
