package projects

import (
	"context"
	"errors"
	"fmt"
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
