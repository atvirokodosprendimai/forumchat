// Package provision owns the one correct way to bring a community into
// existence: insert the community row, seed its undeletable #general channel,
// then add a first, pre-approved member with a given role. Four call sites need
// this exact sequence — the super-admin create, the per-community admin create,
// the SaaS self-serve create and the request-approval create — so the order
// (and the easy-to-forget channel seed) lives here once instead of being copied.
package provision

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
)

// BlobPurger removes a community's uploaded blobs (and rows) before the
// community row is cascade-deleted. *uploads.Store satisfies it; the interface
// keeps internal/uploads out of this package's imports.
type BlobPurger interface {
	DeleteByCommunity(ctx context.Context, communityID string) (int, error)
}

// VectorDropper drops a community's vector collection. *rag.Service satisfies
// it; kept as an interface so provision doesn't import internal/rag.
type VectorDropper interface {
	DropCommunity(ctx context.Context, communityID string) error
}

// Service owns the lifecycle of a community: create (→ seed-channel →
// seed-member) and delete (→ purge blobs → cascade → drop vectors). SeedChannel
// is kept as a func so this package doesn't import internal/chat (which would
// risk an import cycle); main.go wires it from chat.Repo.EnsureDefaultChannel.
type Service struct {
	Communities *community.Repo
	Auth        *auth.Repo
	// SeedChannel seeds the undeletable #general channel for a freshly created
	// community. A runtime-created community is never seen by the boot-time
	// EnsureDefaultChannel sweep, so without this its first chat visit fails with
	// "load channel: sql: no rows in result set". Nil is tolerated (tests).
	SeedChannel func(ctx context.Context, communityID string) error
	// Blobs purges uploaded files on delete. Nil = skip (no blob backend).
	Blobs BlobPurger
	// Vectors drops the community's vector collection on delete. Nil = skip.
	Vectors VectorDropper
	Log     *slog.Logger
}

// Input is one community to create plus the first member to seed.
type Input struct {
	Slug        string
	Name        string
	OwnerUserID string    // the user who becomes the first member
	DisplayName string    // membership display name (usually the email local-part)
	Role        auth.Role // RoleOwner for self-serve/SaaS, RoleAdmin for the legacy super-admin flow
}

// Create runs the create → seed-channel → seed-member sequence. It returns
// community.ErrSlugTaken (unwrapped) when the slug is in use, so callers can map
// it to a friendly message; any other failure is wrapped with context.
func (s *Service) Create(ctx context.Context, in Input) (community.Community, error) {
	c, err := s.Communities.Create(ctx, in.Slug, in.Name)
	if err != nil {
		return community.Community{}, err // includes community.ErrSlugTaken, unwrapped on purpose
	}
	if s.SeedChannel != nil {
		if err := s.SeedChannel(ctx, c.ID); err != nil {
			return community.Community{}, fmt.Errorf("seed default channel: %w", err)
		}
	}
	now := time.Now()
	m := auth.Membership{
		ID:          uuid.NewString(),
		UserID:      in.OwnerUserID,
		CommunityID: c.ID,
		DisplayName: in.DisplayName,
		Role:        in.Role,
		ApprovedAt:  &now,
	}
	if err := s.Auth.CreateMembership(ctx, nil, m); err != nil {
		return community.Community{}, fmt.Errorf("create first membership: %w", err)
	}
	return c, nil
}

// Delete permanently removes a community and ALL of its data. The order matters:
// blobs first (they're enumerated from the upload rows the cascade is about to
// erase), then the community row (FK cascade wipes memberships, chat, threads,
// channels, projects, mailbox … see community.Repo.Delete), then the vector
// collection. The DB delete is the only step that fails loudly; blob and vector
// removal are best-effort (a leftover blob/collection is a leak, not a
// correctness bug) and logged. Callers own confirmation (slug match) + audit.
func (s *Service) Delete(ctx context.Context, communityID string) error {
	if s.Blobs != nil {
		if n, err := s.Blobs.DeleteByCommunity(ctx, communityID); err != nil {
			s.logf("provision: purge community blobs", "community_id", communityID, "err", err, "removed", n)
		}
	}
	if err := s.Communities.Delete(ctx, communityID); err != nil {
		return fmt.Errorf("delete community: %w", err)
	}
	if s.Vectors != nil {
		if err := s.Vectors.DropCommunity(ctx, communityID); err != nil {
			s.logf("provision: drop community vectors", "community_id", communityID, "err", err)
		}
	}
	return nil
}

func (s *Service) logf(msg string, args ...any) {
	if s.Log != nil {
		s.Log.Warn(msg, args...)
	}
}
