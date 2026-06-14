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

var (
	ErrEmptyTitle = errors.New("projects: title required")
	ErrNotFound   = errors.New("projects: not found")
	ErrForbidden  = errors.New("projects: forbidden")
)

// Service composes Repo + Bus into the business-level API used by the
// HTTP handlers. Every mutator publishes a typed Event on success so
// every open SSE stream re-renders just the affected fragment.
type Service struct {
	Repo *Repo
	Bus  *Bus
}

// NewService wraps a repo and bus.
func NewService(repo *Repo, bus *Bus) *Service {
	return &Service{Repo: repo, Bus: bus}
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
