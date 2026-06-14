package projects

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// CreateDiscussionThread persists a new thread. Member or guest.
func (s *Service) CreateDiscussionThread(ctx context.Context, projectID, subject, bodyMD string, creator Identity) (DiscussionThread, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return DiscussionThread{}, ErrEmptyTitle
	}
	if len(subject) > 200 {
		subject = subject[:200]
	}
	html, err := render.RenderMarkdown(strings.TrimSpace(bodyMD))
	if err != nil {
		return DiscussionThread{}, fmt.Errorf("render thread body: %w", err)
	}
	now := time.Now().UTC()
	t := DiscussionThread{
		ID:             uuid.NewString(),
		ProjectID:      projectID,
		Subject:        subject,
		BodyMD:         strings.TrimSpace(bodyMD),
		BodyHTML:       html,
		CreatorUserID:  creator.UserID,
		CreatorGuestID: creator.GuestID,
		CreatorName:    creator.Name,
		LastActivityAt: now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.Repo.InsertDiscussionThread(ctx, t); err != nil {
		return DiscussionThread{}, fmt.Errorf("insert discussion thread: %w", err)
	}
	return t, nil
}

// UpdateDiscussionThread enforces author-or-admin (no grace at the
// thread level — only replies use grace). Soft-delete tombstones can't
// be edited.
func (s *Service) UpdateDiscussionThread(ctx context.Context, projectID, threadID, subject, bodyMD string, caller Identity, callerIsAdmin bool) error {
	t, err := s.Repo.DiscussionThreadByID(ctx, threadID)
	if err != nil {
		return fmt.Errorf("thread lookup: %w", err)
	}
	if t.ProjectID != projectID || t.IsDeleted() {
		return ErrNotFound
	}
	isAuthor := (caller.UserID != "" && t.CreatorUserID == caller.UserID) ||
		(caller.GuestID != "" && t.CreatorGuestID == caller.GuestID)
	if !(callerIsAdmin || isAuthor) {
		return ErrForbidden
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return ErrEmptyTitle
	}
	if len(subject) > 200 {
		subject = subject[:200]
	}
	html, err := render.RenderMarkdown(strings.TrimSpace(bodyMD))
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	return s.Repo.UpdateDiscussionThread(ctx, threadID, subject, strings.TrimSpace(bodyMD), html, time.Now().UTC())
}

// DeleteDiscussionThread soft-deletes. Author OR admin.
func (s *Service) DeleteDiscussionThread(ctx context.Context, projectID, threadID string, caller Identity, callerIsAdmin bool) error {
	t, err := s.Repo.DiscussionThreadByID(ctx, threadID)
	if err != nil {
		return fmt.Errorf("thread lookup: %w", err)
	}
	if t.ProjectID != projectID || t.IsDeleted() {
		return ErrNotFound
	}
	isAuthor := (caller.UserID != "" && t.CreatorUserID == caller.UserID) ||
		(caller.GuestID != "" && t.CreatorGuestID == caller.GuestID)
	if !(callerIsAdmin || isAuthor) {
		return ErrForbidden
	}
	return s.Repo.SoftDeleteDiscussionThread(ctx, threadID, time.Now().UTC())
}
