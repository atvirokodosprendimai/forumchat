package webhooks

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Validation errors surfaced to the admin handler.
var (
	ErrBadDirection = errors.New("webhooks: direction must be 'in' or 'out'")
	ErrBadProvider  = errors.New("webhooks: invalid provider for direction")
	ErrNameRequired = errors.New("webhooks: name is required")
	ErrChannelReq   = errors.New("webhooks: inbound webhook needs a target channel")
	ErrTargetURL    = errors.New("webhooks: outbound webhook needs an http(s) target URL")
)

// inProviders / outProviders are the allowed provider values per direction.
var (
	inProviders  = map[string]bool{"generic": true, "github": true}
	outProviders = map[string]bool{"generic": true, "slack": true, "discord": true}
)

// Service is the write side for webhooks: validation + token minting.
type Service struct{ Repo *Repo }

// NewService returns a Service bound to repo.
func NewService(repo *Repo) *Service { return &Service{Repo: repo} }

// CreateInput carries the admin-supplied fields for a new webhook.
type CreateInput struct {
	CommunityID string
	Direction   string
	Provider    string
	Name        string
	AvatarURL   string
	ChannelID   string // inbound: target ; outbound: "" = all channels
	Secret      string // inbound github signing secret (optional)
	TargetURL   string // outbound destination
	CreatedBy   string
}

// Create validates in and persists a webhook, minting an inbound token.
func (s *Service) Create(ctx context.Context, in CreateInput) (Webhook, error) {
	in.Direction = strings.TrimSpace(in.Direction)
	in.Provider = strings.TrimSpace(in.Provider)
	in.Name = strings.TrimSpace(in.Name)

	if in.Direction != DirIn && in.Direction != DirOut {
		return Webhook{}, ErrBadDirection
	}
	if in.Name == "" {
		return Webhook{}, ErrNameRequired
	}
	if in.Direction == DirIn && !inProviders[in.Provider] {
		return Webhook{}, ErrBadProvider
	}
	if in.Direction == DirOut && !outProviders[in.Provider] {
		return Webhook{}, ErrBadProvider
	}

	w := Webhook{
		ID:          uuid.NewString(),
		CommunityID: in.CommunityID,
		Direction:   in.Direction,
		Provider:    in.Provider,
		Name:        in.Name,
		AvatarURL:   strings.TrimSpace(in.AvatarURL),
		CreatedBy:   in.CreatedBy,
		Enabled:     true,
		CreatedAt:   time.Now(),
	}

	switch in.Direction {
	case DirIn:
		if strings.TrimSpace(in.ChannelID) == "" {
			return Webhook{}, ErrChannelReq
		}
		w.ChannelID = in.ChannelID
		w.Secret = strings.TrimSpace(in.Secret)
		w.Token = newToken()
	case DirOut:
		if !validHTTPURL(in.TargetURL) {
			return Webhook{}, ErrTargetURL
		}
		w.TargetURL = strings.TrimSpace(in.TargetURL)
		w.ChannelID = strings.TrimSpace(in.ChannelID) // "" = all channels
	}

	if err := s.Repo.Create(ctx, w); err != nil {
		return Webhook{}, fmt.Errorf("create webhook: %w", err)
	}
	return w, nil
}

// Rotate replaces an inbound webhook's token and returns the new one.
func (s *Service) Rotate(ctx context.Context, communityID, id string) (string, error) {
	t := newToken()
	if err := s.Repo.RotateToken(ctx, communityID, id, t); err != nil {
		return "", err
	}
	return t, nil
}

// newToken returns a 32-byte URL-safe random token (~43 chars).
func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func validHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
