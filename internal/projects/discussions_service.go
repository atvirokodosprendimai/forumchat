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

// AddDiscussionReply adds a new reply to a thread + bumps the thread's
// last_activity_at so it floats up in the list.
func (s *Service) AddDiscussionReply(ctx context.Context, projectID, threadID, quotedReplyID, bodyMD string, author Identity) (DiscussionReply, error) {
	t, err := s.Repo.DiscussionThreadByID(ctx, threadID)
	if err != nil {
		return DiscussionReply{}, fmt.Errorf("thread lookup: %w", err)
	}
	if t.ProjectID != projectID || t.IsDeleted() {
		return DiscussionReply{}, ErrNotFound
	}
	bodyMD = strings.TrimSpace(bodyMD)
	if bodyMD == "" {
		return DiscussionReply{}, ErrEmptyTitle
	}
	html, err := render.RenderMarkdown(bodyMD)
	if err != nil {
		return DiscussionReply{}, fmt.Errorf("render: %w", err)
	}
	// Validate quoted_reply_id is in the same thread (no cross-thread quoting).
	if quotedReplyID != "" {
		q, err := s.Repo.DiscussionReplyByID(ctx, quotedReplyID)
		if err != nil || q.ThreadID != threadID || q.IsDeleted() {
			quotedReplyID = ""
		}
	}
	now := time.Now().UTC()
	rr := DiscussionReply{
		ID:            uuid.NewString(),
		ThreadID:      threadID,
		QuotedReplyID: quotedReplyID,
		AuthorUserID:  author.UserID,
		AuthorGuestID: author.GuestID,
		AuthorName:    author.Name,
		BodyMD:        bodyMD,
		BodyHTML:      html,
		CreatedAt:     now,
	}
	if err := s.Repo.InsertDiscussionReply(ctx, rr); err != nil {
		return DiscussionReply{}, fmt.Errorf("insert reply: %w", err)
	}
	_ = s.Repo.BumpDiscussionThreadActivity(ctx, threadID, now)
	return rr, nil
}

// UpdateDiscussionReply enforces grace window + author-or-admin.
func (s *Service) UpdateDiscussionReply(ctx context.Context, projectID, threadID, replyID, bodyMD string, caller Identity, callerIsAdmin bool) error {
	rr, err := s.Repo.DiscussionReplyByID(ctx, replyID)
	if err != nil {
		return fmt.Errorf("reply lookup: %w", err)
	}
	if rr.ThreadID != threadID || rr.IsDeleted() {
		return ErrNotFound
	}
	t, err := s.Repo.DiscussionThreadByID(ctx, threadID)
	if err != nil || t.ProjectID != projectID || t.IsDeleted() {
		return ErrNotFound
	}
	now := time.Now().UTC()
	isAuthor := (caller.UserID != "" && rr.AuthorUserID == caller.UserID) ||
		(caller.GuestID != "" && rr.AuthorGuestID == caller.GuestID)
	if !(callerIsAdmin || (isAuthor && now.Sub(rr.CreatedAt) <= s.EditGrace)) {
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
	return s.Repo.UpdateDiscussionReply(ctx, replyID, bodyMD, html, now)
}

// DeleteDiscussionReply soft-deletes. Author (anytime) OR admin.
func (s *Service) DeleteDiscussionReply(ctx context.Context, projectID, threadID, replyID string, caller Identity, callerIsAdmin bool) error {
	rr, err := s.Repo.DiscussionReplyByID(ctx, replyID)
	if err != nil {
		return fmt.Errorf("reply lookup: %w", err)
	}
	if rr.ThreadID != threadID || rr.IsDeleted() {
		return ErrNotFound
	}
	t, err := s.Repo.DiscussionThreadByID(ctx, threadID)
	if err != nil || t.ProjectID != projectID || t.IsDeleted() {
		return ErrNotFound
	}
	isAuthor := (caller.UserID != "" && rr.AuthorUserID == caller.UserID) ||
		(caller.GuestID != "" && rr.AuthorGuestID == caller.GuestID)
	if !(callerIsAdmin || isAuthor) {
		return ErrForbidden
	}
	return s.Repo.SoftDeleteDiscussionReply(ctx, replyID, time.Now().UTC())
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
