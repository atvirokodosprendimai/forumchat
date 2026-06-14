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
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
)

var (
	ErrEmptyTitle = errors.New("projects: title required")
	ErrNotFound   = errors.New("projects: not found")
	ErrForbidden  = errors.New("projects: forbidden")
)

// Service composes Repo + Bus + uploads.Store into the business-level
// API used by the HTTP handlers. Every mutator publishes a typed Event
// on success so every open SSE stream re-renders just the affected
// fragment.
type Service struct {
	Repo      *Repo
	Bus       *Bus
	Uploads   *uploads.Store
	EditGrace time.Duration
}

// NewService wraps a repo, bus, and uploads store. EditGrace controls
// how long after creation an author can still edit/delete their own
// comment (matches forum.Service.EditGrace semantically).
func NewService(repo *Repo, bus *Bus, store *uploads.Store, editGrace time.Duration) *Service {
	return &Service{Repo: repo, Bus: bus, Uploads: store, EditGrace: editGrace}
}

// CreateProject persists a fresh project with rendered description HTML.
func (s *Service) CreateProject(ctx context.Context, communityID, creatorID, title, descMD string) (Project, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Project{}, ErrEmptyTitle
	}
	if len(title) > 200 {
		title = title[:200]
	}
	html, err := render.RenderMarkdown(strings.TrimSpace(descMD))
	if err != nil {
		return Project{}, fmt.Errorf("render description: %w", err)
	}
	now := time.Now().UTC()
	p := Project{
		ID:              uuid.NewString(),
		CommunityID:     communityID,
		CreatorUserID:   creatorID,
		Title:           title,
		DescriptionMD:   strings.TrimSpace(descMD),
		DescriptionHTML: html,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.Repo.Insert(ctx, p); err != nil {
		return Project{}, fmt.Errorf("insert project: %w", err)
	}
	return p, nil
}

// UpdateTitle persists a new title and publishes a header event.
func (s *Service) UpdateTitle(ctx context.Context, projectID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return ErrEmptyTitle
	}
	if len(title) > 200 {
		title = title[:200]
	}
	if err := s.Repo.UpdateTitle(ctx, projectID, title, time.Now().UTC()); err != nil {
		return fmt.Errorf("update title: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "header"})
	return nil
}

// UpdateDescription re-renders markdown and persists both forms, then
// publishes a header event.
func (s *Service) UpdateDescription(ctx context.Context, projectID, descMD string) error {
	descMD = strings.TrimSpace(descMD)
	html, err := render.RenderMarkdown(descMD)
	if err != nil {
		return fmt.Errorf("render description: %w", err)
	}
	if err := s.Repo.UpdateDescription(ctx, projectID, descMD, html, time.Now().UTC()); err != nil {
		return fmt.Errorf("update description: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "header"})
	return nil
}

// AddTodo appends a row at the end of the checklist.
func (s *Service) AddTodo(ctx context.Context, projectID, creatorID, body string) (Todo, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Todo{}, ErrEmptyTitle
	}
	if len(body) > 500 {
		body = body[:500]
	}
	maxOrder, err := s.Repo.MaxTodoSortOrder(ctx, projectID)
	if err != nil {
		return Todo{}, fmt.Errorf("max sort: %w", err)
	}
	now := time.Now().UTC()
	t := Todo{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		Body:      body,
		SortOrder: maxOrder + 1,
		CreatedBy: creatorID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.Repo.InsertTodo(ctx, t); err != nil {
		return Todo{}, fmt.Errorf("insert todo: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "todos"})
	return t, nil
}

// UpdateTodoBody replaces the text of one todo.
func (s *Service) UpdateTodoBody(ctx context.Context, projectID, todoID, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return ErrEmptyTitle
	}
	if len(body) > 500 {
		body = body[:500]
	}
	if err := s.Repo.UpdateTodoBody(ctx, todoID, body, time.Now().UTC()); err != nil {
		return fmt.Errorf("update todo body: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "todos"})
	return nil
}

// ToggleTodo flips done and publishes.
func (s *Service) ToggleTodo(ctx context.Context, projectID, todoID string) error {
	if err := s.Repo.ToggleTodoDone(ctx, todoID, time.Now().UTC()); err != nil {
		return fmt.Errorf("toggle todo: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "todos"})
	return nil
}

// DeleteTodo removes one row.
func (s *Service) DeleteTodo(ctx context.Context, projectID, todoID string) error {
	if err := s.Repo.DeleteTodo(ctx, todoID); err != nil {
		return fmt.Errorf("delete todo: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "todos"})
	return nil
}

// ReorderTodos applies a new ordering.
func (s *Service) ReorderTodos(ctx context.Context, projectID string, order []string) error {
	if err := s.Repo.ReorderTodos(ctx, projectID, order); err != nil {
		return fmt.Errorf("reorder todos: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "todos"})
	return nil
}

// AddAttachment persists a multipart-uploaded file in uploads + a
// pointer row in project_attachments, then publishes.
func (s *Service) AddAttachment(ctx context.Context, projectID, communityID, uploaderID, mime, filename, category string, body io.Reader) (Attachment, error) {
	if filename = strings.TrimSpace(filename); filename == "" {
		filename = "file"
	}
	category = strings.TrimSpace(category)
	if category == "" {
		category = "common"
	}
	u, err := s.Uploads.SaveAttachment(ctx, uploaderID, communityID, mime, filename, body)
	if err != nil {
		return Attachment{}, fmt.Errorf("upload save: %w", err)
	}
	a := Attachment{
		ID:         uuid.NewString(),
		ProjectID:  projectID,
		UploadID:   u.ID,
		Filename:   filename,
		MIME:       u.MIME,
		SizeBytes:  u.Size,
		UploaderID: uploaderID,
		Category:   category,
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.Repo.InsertAttachment(ctx, a); err != nil {
		return Attachment{}, fmt.Errorf("insert attachment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "attachments"})
	return a, nil
}

// Archive flips archived_at to now. Caller is project creator or
// community admin. Idempotent (re-archives bump updated_at).
func (s *Service) Archive(ctx context.Context, projectID, callerUserID string, callerIsAdmin bool) error {
	return s.toggleArchive(ctx, projectID, callerUserID, callerIsAdmin, true)
}

// Unarchive clears archived_at.
func (s *Service) Unarchive(ctx context.Context, projectID, callerUserID string, callerIsAdmin bool) error {
	return s.toggleArchive(ctx, projectID, callerUserID, callerIsAdmin, false)
}

func (s *Service) toggleArchive(ctx context.Context, projectID, callerUserID string, callerIsAdmin bool, archive bool) error {
	p, err := s.Repo.ByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project lookup: %w", err)
	}
	if !(callerIsAdmin || p.CreatorUserID == callerUserID) {
		return ErrForbidden
	}
	now := time.Now().UTC()
	var at *time.Time
	if archive {
		at = &now
	}
	if err := s.Repo.SetArchived(ctx, projectID, at, now); err != nil {
		return fmt.Errorf("set archived: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "archive"})
	return nil
}

// DeleteProject hard-deletes; FK cascade drops todos/attachments/
// comments. Each attachment's underlying uploads row is cleaned up
// first so the file dedupe count stays correct.
func (s *Service) DeleteProject(ctx context.Context, projectID, callerUserID string, callerIsAdmin bool) error {
	p, err := s.Repo.ByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project lookup: %w", err)
	}
	if !(callerIsAdmin || p.CreatorUserID == callerUserID) {
		return ErrForbidden
	}
	atts, err := s.Repo.ListAttachments(ctx, projectID)
	if err == nil {
		for _, a := range atts {
			_ = s.Uploads.Delete(ctx, a.UploadID)
		}
	}
	if err := s.Repo.Delete(ctx, projectID); err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	return nil
}

// AddComment renders markdown and persists one new comment.
func (s *Service) AddComment(ctx context.Context, projectID, authorID, bodyMD string) (Comment, error) {
	bodyMD = strings.TrimSpace(bodyMD)
	if bodyMD == "" {
		return Comment{}, ErrEmptyTitle
	}
	html, err := render.RenderMarkdown(bodyMD)
	if err != nil {
		return Comment{}, fmt.Errorf("render comment: %w", err)
	}
	c := Comment{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		AuthorID:  authorID,
		BodyMD:    bodyMD,
		BodyHTML:  html,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.Repo.InsertComment(ctx, c); err != nil {
		return Comment{}, fmt.Errorf("insert comment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "comments"})
	return c, nil
}

// UpdateComment enforces the edit grace window + author-or-admin rule.
func (s *Service) UpdateComment(ctx context.Context, projectID, commentID, callerUserID string, callerIsAdmin bool, bodyMD string) error {
	c, err := s.Repo.CommentByID(ctx, commentID)
	if err != nil {
		return fmt.Errorf("comment lookup: %w", err)
	}
	if c.ProjectID != projectID || c.IsDeleted() {
		return ErrNotFound
	}
	now := time.Now().UTC()
	if !(callerIsAdmin || (c.AuthorID == callerUserID && now.Sub(c.CreatedAt) <= s.EditGrace)) {
		return ErrForbidden
	}
	bodyMD = strings.TrimSpace(bodyMD)
	if bodyMD == "" {
		return ErrEmptyTitle
	}
	html, err := render.RenderMarkdown(bodyMD)
	if err != nil {
		return fmt.Errorf("render comment: %w", err)
	}
	if err := s.Repo.UpdateComment(ctx, commentID, bodyMD, html, now); err != nil {
		return fmt.Errorf("update comment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "comments"})
	return nil
}

// DeleteComment soft-deletes with the same author-or-admin rule as
// UpdateComment, but skips the grace check — authors can always remove
// their own comments. Admins can delete anyone's.
func (s *Service) DeleteComment(ctx context.Context, projectID, commentID, callerUserID string, callerIsAdmin bool) error {
	c, err := s.Repo.CommentByID(ctx, commentID)
	if err != nil {
		return fmt.Errorf("comment lookup: %w", err)
	}
	if c.ProjectID != projectID || c.IsDeleted() {
		return ErrNotFound
	}
	if !(callerIsAdmin || c.AuthorID == callerUserID) {
		return ErrForbidden
	}
	if err := s.Repo.SoftDeleteComment(ctx, commentID, time.Now().UTC()); err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	s.Bus.PublishProject(projectID, Event{Kind: "comments"})
	return nil
}

// DeleteAttachment enforces permission then deletes both rows. Caller
// supplies the requester identity so we can authorize. Returns
// ErrForbidden if neither uploader, creator, nor admin.
func (s *Service) DeleteAttachment(ctx context.Context, projectID, attachmentID, callerUserID string, callerIsAdmin bool) error {
	a, err := s.Repo.AttachmentByID(ctx, attachmentID)
	if err != nil {
		return fmt.Errorf("attachment lookup: %w", err)
	}
	p, err := s.Repo.ByID(ctx, projectID)
	if err != nil {
		return fmt.Errorf("project lookup: %w", err)
	}
	if a.ProjectID != projectID {
		return ErrNotFound
	}
	if !(callerIsAdmin || a.UploaderID == callerUserID || p.CreatorUserID == callerUserID) {
		return ErrForbidden
	}
	if err := s.Repo.DeleteAttachment(ctx, attachmentID); err != nil {
		return fmt.Errorf("delete attachment: %w", err)
	}
	// Best-effort underlying-file cleanup. uploads.Store.Delete only
	// removes the file when no other row references the same content
	// hash, so this is safe to call unconditionally.
	_ = s.Uploads.Delete(ctx, a.UploadID)
	s.Bus.PublishProject(projectID, Event{Kind: "attachments"})
	return nil
}
