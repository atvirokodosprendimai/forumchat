package connectors

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/chat"
)

// Validation errors surfaced to the admin UI.
var (
	ErrEmptyName = errors.New("connectors: name required")
)

// MemberFactory provisions and tears down the synthetic member a connector acts
// as. It is the consumer-side seam (AGENTS §6b) over auth.Service, declared here
// so connectors never imports auth for writes; auth.Service satisfies it
// directly (method names match) and is wired in main.go.
type MemberFactory interface {
	CreateServiceAccount(ctx context.Context, communityID, displayName, avatar string) (userID string, err error)
	RenameServiceMember(ctx context.Context, userID, displayName, avatar string) error
	RemoveServiceAccount(ctx context.Context, userID string) error
}

// Service is the single writer for connectors: it mints secrets, provisions the
// synthetic member, validates the channel allowlist + capability set, and
// persists. It holds *chat.Repo only to validate that requested channels belong
// to the connector's community (a one-way import, like webhooks → chat).
type Service struct {
	Repo     *Repo
	Members  MemberFactory
	ChatRepo *chat.Repo
}

// NewService returns a Service.
func NewService(repo *Repo, members MemberFactory, chatRepo *chat.Repo) *Service {
	return &Service{Repo: repo, Members: members, ChatRepo: chatRepo}
}

// CreateInput is the validated request to create a connector. ChannelIDs is the
// allowlist (empty = all channels); Capabilities is the granted power set.
type CreateInput struct {
	CommunityID  string
	Name         string
	AvatarURL    string
	ChannelIDs   []string
	Capabilities []string
	MentionsOnly bool
	CreatedBy    string
}

// Create provisions the synthetic member, mints a secret, and persists the
// connector + its (validated) channel allowlist. The returned Connector carries
// the freshly minted Secret so the handler can reveal it once.
func (s *Service) Create(ctx context.Context, in CreateInput) (Connector, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Connector{}, ErrEmptyName
	}
	channelIDs, err := s.validChannels(ctx, in.CommunityID, in.ChannelIDs)
	if err != nil {
		return Connector{}, err
	}
	secret, err := mintSecret()
	if err != nil {
		return Connector{}, err
	}
	// Provision the member first so the connectors.user_id FK holds.
	userID, err := s.Members.CreateServiceAccount(ctx, in.CommunityID, name, in.AvatarURL)
	if err != nil {
		return Connector{}, fmt.Errorf("provision member: %w", err)
	}
	c := Connector{
		ID:           uuid.NewString(),
		CommunityID:  in.CommunityID,
		UserID:       userID,
		Name:         name,
		AvatarURL:    in.AvatarURL,
		Secret:       secret,
		Capabilities: normalizeCapabilities(in.Capabilities),
		MentionsOnly: in.MentionsOnly,
		Enabled:      true,
		CreatedBy:    in.CreatedBy,
		CreatedAt:    time.Now(),
	}
	if err := s.Repo.Create(ctx, c); err != nil {
		// Best-effort rollback of the orphaned member so a failed create doesn't
		// leave a ghost in the roster.
		_ = s.Members.RemoveServiceAccount(ctx, userID)
		return Connector{}, fmt.Errorf("insert connector: %w", err)
	}
	if err := s.Repo.SetChannels(ctx, c.ID, channelIDs); err != nil {
		return Connector{}, fmt.Errorf("set channels: %w", err)
	}
	return c, nil
}

// UpdateInput edits a connector's display + scope + grants in one call.
type UpdateInput struct {
	CommunityID  string
	ID           string
	Name         string
	AvatarURL    string
	ChannelIDs   []string
	Capabilities []string
	MentionsOnly bool
}

// Update applies metadata, capability, channel, and member-rename changes. The
// secret is untouched (rotate is a separate, explicit action).
func (s *Service) Update(ctx context.Context, in UpdateInput) error {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return ErrEmptyName
	}
	c, err := s.Repo.byIDInCommunity(ctx, in.CommunityID, in.ID)
	if err != nil {
		return err
	}
	channelIDs, err := s.validChannels(ctx, in.CommunityID, in.ChannelIDs)
	if err != nil {
		return err
	}
	if err := s.Repo.SetMeta(ctx, in.CommunityID, in.ID, name, in.AvatarURL, in.MentionsOnly); err != nil {
		return err
	}
	if err := s.Repo.SetCapabilities(ctx, in.CommunityID, in.ID, normalizeCapabilities(in.Capabilities)); err != nil {
		return err
	}
	if err := s.Repo.SetChannels(ctx, in.ID, channelIDs); err != nil {
		return err
	}
	return s.Members.RenameServiceMember(ctx, c.UserID, name, in.AvatarURL)
}

// Rotate mints a fresh secret for a connector and returns it (revealed once).
// Invalidates the old stream URL and every prior body signature at once.
func (s *Service) Rotate(ctx context.Context, communityID, id string) (string, error) {
	secret, err := mintSecret()
	if err != nil {
		return "", err
	}
	if err := s.Repo.RotateSecret(ctx, communityID, id, secret); err != nil {
		return "", err
	}
	return secret, nil
}

// Delete removes a connector and its synthetic member. The member's authored
// messages survive as a "deleted member" (author_id SET NULL).
func (s *Service) Delete(ctx context.Context, communityID, id string) error {
	c, err := s.Repo.byIDInCommunity(ctx, communityID, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // already gone — idempotent
		}
		return err
	}
	if err := s.Repo.Delete(ctx, communityID, id); err != nil {
		return err
	}
	return s.Members.RemoveServiceAccount(ctx, c.UserID)
}

// validChannels filters requested ids down to channels that actually belong to
// communityID, so a forged id can never attach a connector to another tenant's
// channel. An empty request stays empty (= all channels).
func (s *Service) validChannels(ctx context.Context, communityID string, requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, nil
	}
	channels, err := s.ChatRepo.ListChannels(ctx, communityID, true)
	if err != nil {
		return nil, fmt.Errorf("load channels: %w", err)
	}
	valid := make(map[string]bool, len(channels))
	for _, ch := range channels {
		valid[ch.ID] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range requested {
		if valid[id] && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// mintSecret returns 32 bytes of crypto/rand as base64url (~43 chars). This is
// the per-connector HMAC key — high-entropy, stored plaintext (it IS the
// secret), revealed on create/rotate.
func mintSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mint secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
