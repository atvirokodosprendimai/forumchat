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
