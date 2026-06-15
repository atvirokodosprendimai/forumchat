package projects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// ErrBadStatus is returned when a status string is not in IssueStatuses.
var ErrBadStatus = errors.New("projects: bad issue status")

// validStatus reports whether s is one of the canonical statuses.
func validStatus(s string) bool {
	for _, v := range IssueStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// CreateIssue persists a fresh issue + publishes both the per-issue
// "issue" event AND a project-wide issues-list event so the project
// page's issues panel updates.
func (s *Service) CreateIssue(ctx context.Context, projectID, title, bodyMD string, creator Identity) (Issue, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Issue{}, ErrEmptyTitle
	}
	if len(title) > 200 {
		title = title[:200]
	}
	html, err := render.RenderMarkdown(strings.TrimSpace(bodyMD))
	if err != nil {
		return Issue{}, fmt.Errorf("render issue body: %w", err)
	}
	now := time.Now().UTC()
	i := Issue{
		ID:             uuid.NewString(),
		ProjectID:      projectID,
		Title:          title,
		BodyMD:         strings.TrimSpace(bodyMD),
		BodyHTML:       html,
		Status:         IssueOpen,
		CreatorUserID:  creator.UserID,
		CreatorGuestID: creator.GuestID,
		CreatorName:    creator.Name,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.Repo.InsertIssue(ctx, i); err != nil {
		return Issue{}, fmt.Errorf("insert issue: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return i, nil
}

// UpdateIssueStatus advances the status. Member-only — guests can't
// move the workflow forward.
func (s *Service) UpdateIssueStatus(ctx context.Context, projectID, issueID, status string, caller Identity) error {
	if caller.IsGuest() {
		return ErrForbidden
	}
	if !validStatus(status) {
		return ErrBadStatus
	}
	if err := s.Repo.UpdateIssueStatus(ctx, issueID, status, time.Now().UTC()); err != nil {
		return fmt.Errorf("update status: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return nil
}

// UpdateIssueTitle replaces the title — author OR admin.
func (s *Service) UpdateIssueTitle(ctx context.Context, projectID, issueID, title string, caller Identity, callerIsAdmin bool) error {
	i, err := s.Repo.IssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("issue lookup: %w", err)
	}
	if i.ProjectID != projectID {
		return ErrNotFound
	}
	if !(callerIsAdmin || (caller.UserID != "" && i.CreatorUserID == caller.UserID) ||
		(caller.GuestID != "" && i.CreatorGuestID == caller.GuestID)) {
		return ErrForbidden
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrEmptyTitle
	}
	if len(title) > 200 {
		title = title[:200]
	}
	if err := s.Repo.UpdateIssueTitle(ctx, issueID, title, time.Now().UTC()); err != nil {
		return fmt.Errorf("update title: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return nil
}

// UpdateIssueBody replaces the body. Author OR admin.
func (s *Service) UpdateIssueBody(ctx context.Context, projectID, issueID, bodyMD string, caller Identity, callerIsAdmin bool) error {
	i, err := s.Repo.IssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("issue lookup: %w", err)
	}
	if i.ProjectID != projectID {
		return ErrNotFound
	}
	if !(callerIsAdmin || (caller.UserID != "" && i.CreatorUserID == caller.UserID) ||
		(caller.GuestID != "" && i.CreatorGuestID == caller.GuestID)) {
		return ErrForbidden
	}
	html, err := render.RenderMarkdown(strings.TrimSpace(bodyMD))
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if err := s.Repo.UpdateIssueBody(ctx, issueID, strings.TrimSpace(bodyMD), html, time.Now().UTC()); err != nil {
		return fmt.Errorf("update body: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return nil
}

// AddIssueComment persists a new comment + publishes.
func (s *Service) AddIssueComment(ctx context.Context, projectID, issueID string, author Identity, bodyMD string) (IssueComment, error) {
	bodyMD = strings.TrimSpace(bodyMD)
	if bodyMD == "" {
		return IssueComment{}, ErrEmptyTitle
	}
	html, err := render.RenderMarkdown(bodyMD)
	if err != nil {
		return IssueComment{}, fmt.Errorf("render: %w", err)
	}
	c := IssueComment{
		ID:            uuid.NewString(),
		IssueID:       issueID,
		AuthorUserID:  author.UserID,
		AuthorGuestID: author.GuestID,
		AuthorName:    author.Name,
		BodyMD:        bodyMD,
		BodyHTML:      html,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.Repo.InsertIssueComment(ctx, c); err != nil {
		return IssueComment{}, fmt.Errorf("insert issue comment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return c, nil
}

// UpdateIssueComment edits with grace window enforcement.
func (s *Service) UpdateIssueComment(ctx context.Context, projectID, issueID, commentID string, caller Identity, callerIsAdmin bool, bodyMD string) error {
	c, err := s.Repo.IssueCommentByID(ctx, commentID)
	if err != nil {
		return fmt.Errorf("issue comment lookup: %w", err)
	}
	if c.IssueID != issueID || c.IsDeleted() {
		return ErrNotFound
	}
	now := time.Now().UTC()
	isAuthor := (caller.UserID != "" && c.AuthorUserID == caller.UserID) ||
		(caller.GuestID != "" && c.AuthorGuestID == caller.GuestID)
	if !(callerIsAdmin || (isAuthor && now.Sub(c.CreatedAt) <= s.EditGrace)) {
		return ErrForbidden
	}
	bodyMD = strings.TrimSpace(bodyMD)
	if bodyMD == "" {
		return ErrEmptyTitle
	}
	html, err := render.RenderMarkdown(bodyMD)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	if err := s.Repo.UpdateIssueComment(ctx, commentID, bodyMD, html, now); err != nil {
		return fmt.Errorf("update issue comment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return nil
}

// DeleteIssueComment soft-deletes.
func (s *Service) DeleteIssueComment(ctx context.Context, projectID, issueID, commentID string, caller Identity, callerIsAdmin bool) error {
	c, err := s.Repo.IssueCommentByID(ctx, commentID)
	if err != nil {
		return fmt.Errorf("issue comment lookup: %w", err)
	}
	if c.IssueID != issueID || c.IsDeleted() {
		return ErrNotFound
	}
	isAuthor := (caller.UserID != "" && c.AuthorUserID == caller.UserID) ||
		(caller.GuestID != "" && c.AuthorGuestID == caller.GuestID)
	if !(callerIsAdmin || isAuthor) {
		return ErrForbidden
	}
	if err := s.Repo.SoftDeleteIssueComment(ctx, commentID, time.Now().UTC()); err != nil {
		return fmt.Errorf("delete issue comment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return nil
}

// AddIssueAttachment uploads a file (any MIME) via uploads.Store.
// SaveAttachment and links it to the issue. commentID may be empty
// to attach to the issue body itself. Previously gated by Save's
// image-only whitelist; widened so the email-ingest auto-issue
// flow can attach PDFs, SVGs, ZIPs, etc.
//
// filename matters for non-image MIMEs (download Content-Disposition);
// pass "" for paste-image style flows and a fallback "file" is used.
//
// uploads.owner_id has a NOT NULL FK on users(id) — guests aren't in
// the users table, so we attribute their uploads to the project
// creator instead. The real uploader identity (guest or auth) is still
// captured on project_issue_attachments.uploader_user_id /
// uploader_guest_id so audit + permission checks aren't lost.
func (s *Service) AddIssueAttachment(ctx context.Context, projectID, issueID, commentID, communityID, mime, filename string, body io.Reader, uploader Identity) (IssueAttachment, error) {
	ownerID := uploader.UserID
	if ownerID == "" {
		p, err := s.Repo.ByID(ctx, projectID)
		if err != nil {
			return IssueAttachment{}, fmt.Errorf("project lookup: %w", err)
		}
		ownerID = p.CreatorUserID
	}
	u, err := s.Uploads.SaveAttachment(ctx, ownerID, communityID, mime, filename, body)
	if err != nil {
		return IssueAttachment{}, fmt.Errorf("upload save: %w", err)
	}
	a := IssueAttachment{
		ID:              uuid.NewString(),
		IssueID:         issueID,
		CommentID:       commentID,
		UploadID:        u.ID,
		UploaderUserID:  uploader.UserID,
		UploaderGuestID: uploader.GuestID,
		UploaderName:    uploader.Name,
		CreatedAt:       time.Now().UTC(),
	}
	if err := s.Repo.InsertIssueAttachment(ctx, a); err != nil {
		return IssueAttachment{}, fmt.Errorf("insert issue attachment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return a, nil
}

// DeleteIssueAttachment removes the row + (potentially) the underlying
// file (uploads.Store.Delete no-ops when other rows still reference the
// same content hash). Uploader OR admin.
func (s *Service) DeleteIssueAttachment(ctx context.Context, projectID, issueID, attID string, caller Identity, callerIsAdmin bool) error {
	a, err := s.Repo.IssueAttachmentByID(ctx, attID)
	if err != nil {
		return fmt.Errorf("attachment lookup: %w", err)
	}
	if a.IssueID != issueID {
		return ErrNotFound
	}
	isUploader := (caller.UserID != "" && a.UploaderUserID == caller.UserID) ||
		(caller.GuestID != "" && a.UploaderGuestID == caller.GuestID)
	if !(callerIsAdmin || isUploader) {
		return ErrForbidden
	}
	if err := s.Repo.DeleteIssueAttachment(ctx, attID); err != nil {
		return fmt.Errorf("delete issue attachment: %w", err)
	}
	_ = s.Uploads.Delete(ctx, a.UploadID)
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return nil
}

// DeleteIssue removes an issue. Author OR admin.
func (s *Service) DeleteIssue(ctx context.Context, projectID, issueID string, caller Identity, callerIsAdmin bool) error {
	i, err := s.Repo.IssueByID(ctx, issueID)
	if err != nil {
		return fmt.Errorf("issue lookup: %w", err)
	}
	if i.ProjectID != projectID {
		return ErrNotFound
	}
	if !(callerIsAdmin || (caller.UserID != "" && i.CreatorUserID == caller.UserID) ||
		(caller.GuestID != "" && i.CreatorGuestID == caller.GuestID)) {
		return ErrForbidden
	}
	if err := s.Repo.DeleteIssue(ctx, issueID); err != nil {
		return fmt.Errorf("delete issue: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "issues"})
	return nil
}
