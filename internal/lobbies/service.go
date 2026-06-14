package lobbies

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// ErrClosedOrExpired is returned by Send and Join when the lobby has
// transitioned out of the open state (or its expires_at has passed).
// Distinct from ErrNotFound so handlers can return 410 vs 404.
var ErrClosedOrExpired = errors.New("lobby is closed or expired")

// ErrEmptyBody is returned by Send when both the markdown body and any
// inlined image data are missing. Mirrors chat's rule: empty messages
// are silently dropped at the handler boundary, but the service layer
// surfaces the error for the test path.
var ErrEmptyBody = errors.New("message body is empty")

// ErrPromoteNeedsEmail is returned by Promote when the lobby was minted
// without an email and the guest never supplied one. Promote-to-member
// uses email as the invite-binding key.
var ErrPromoteNeedsEmail = errors.New("guest email required for promotion")

// InviteIssuer is the slice of auth.Service that Promote depends on.
// Defining the interface locally lets the lobbies package stay free of
// an auth import in tests while still wiring to auth.Service in main.
type InviteIssuer interface {
	IssueInvite(ctx context.Context, communityID string, createdBy *string, maxUses *int) (string, error)
}

// Service wraps the Repo with the business logic the HTTP handlers
// need: token minting, message rendering, state-machine transitions,
// promote-to-member.
type Service struct {
	Repo    *Repo
	Invites InviteIssuer
}

func NewService(repo *Repo, invites InviteIssuer) *Service {
	return &Service{Repo: repo, Invites: invites}
}

// MintInput captures the host-side form for creating a lobby.
type MintInput struct {
	CommunityID    string
	HostUserID     string
	Medium         string        // MediumLobby | MediumRoom
	GuestName      string        // optional pre-fill
	GuestEmail     string        // optional, enables Promote later
	ExpiresIn      time.Duration // zero = no expiry
}

// Mint produces a new lobby with a fresh random token. Retries the
// token mint up to 5 times on the unlikely UNIQUE collision before
// giving up — 32 bytes of entropy makes this practically impossible
// but we still cap the retry budget.
func (s *Service) Mint(ctx context.Context, in MintInput) (Lobby, error) {
	medium := in.Medium
	if medium == "" {
		medium = MediumLobby
	}
	if medium != MediumLobby && medium != MediumRoom {
		return Lobby{}, fmt.Errorf("unknown medium %q", medium)
	}
	now := time.Now()
	for attempt := 0; attempt < 5; attempt++ {
		token, err := randomToken(32)
		if err != nil {
			return Lobby{}, err
		}
		l := Lobby{
			ID:               uuid.NewString(),
			CommunityID:      in.CommunityID,
			HostUserID:       in.HostUserID,
			Medium:           medium,
			GuestDisplayName: strings.TrimSpace(in.GuestName),
			GuestEmail:       strings.TrimSpace(in.GuestEmail),
			GuestToken:       token,
			Status:           StatusOpen,
			CreatedAt:        now,
			LastActivityAt:   now,
		}
		if in.ExpiresIn != 0 {
			exp := now.Add(in.ExpiresIn)
			l.ExpiresAt = &exp
		}
		err = s.Repo.Create(ctx, l)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, ErrTokenTaken) {
			return Lobby{}, err
		}
	}
	return Lobby{}, fmt.Errorf("mint: exhausted token retries")
}

// SendInput is the message-write contract. AuthorKind switches between
// AuthorHost and AuthorGuest; the handler picks the right one. For
// host messages AuthorUserID is the host's user id; for guest messages
// it stays nil (guests have no account).
type SendInput struct {
	LobbyID      string
	AuthorKind   string
	AuthorUserID *string
	BodyMarkdown string
}

// Send validates state, renders markdown, persists the message, bumps
// activity. Caller is responsible for any broadcast fanout (NATS, SSE)
// after this returns.
func (s *Service) Send(ctx context.Context, in SendInput) (LobbyMessage, error) {
	body := strings.TrimSpace(in.BodyMarkdown)
	if body == "" {
		return LobbyMessage{}, ErrEmptyBody
	}
	if in.AuthorKind != AuthorHost && in.AuthorKind != AuthorGuest {
		return LobbyMessage{}, fmt.Errorf("unknown author_kind %q", in.AuthorKind)
	}
	l, err := s.Repo.ByID(ctx, in.LobbyID)
	if err != nil {
		return LobbyMessage{}, err
	}
	if !l.IsOpen(time.Now()) {
		return LobbyMessage{}, ErrClosedOrExpired
	}
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return LobbyMessage{}, fmt.Errorf("render: %w", err)
	}
	m := LobbyMessage{
		ID:           uuid.NewString(),
		LobbyID:      l.ID,
		AuthorKind:   in.AuthorKind,
		AuthorUserID: in.AuthorUserID,
		BodyMarkdown: body,
		BodyHTML:     html,
		CreatedAt:    time.Now(),
	}
	if err := s.Repo.AppendMessage(ctx, m); err != nil {
		return LobbyMessage{}, err
	}
	if err := s.Repo.TouchActivity(ctx, l.ID); err != nil {
		// Non-fatal: the message persisted; activity sort is a UX nicety.
		// Surface to the caller for logging.
		return m, err
	}
	return m, nil
}

// Join captures the guest's chosen display name on first arrival.
// Idempotent — calling twice with the same name is a no-op. Refuses
// closed/expired lobbies so the guest can't keep typing into a dead
// thread.
func (s *Service) Join(ctx context.Context, token, guestName string) (Lobby, error) {
	l, err := s.Repo.ByToken(ctx, token)
	if err != nil {
		return Lobby{}, err
	}
	if !l.IsOpen(time.Now()) {
		return Lobby{}, ErrClosedOrExpired
	}
	name := strings.TrimSpace(guestName)
	if name == "" {
		// Allow blank join when host pre-filled the name at mint time.
		if l.GuestDisplayName == "" {
			return Lobby{}, fmt.Errorf("display name required")
		}
		return l, nil
	}
	if name == l.GuestDisplayName {
		return l, nil
	}
	if err := s.Repo.UpdateGuestProfile(ctx, l.ID, name, l.GuestEmail); err != nil {
		return Lobby{}, err
	}
	l.GuestDisplayName = name
	return l, nil
}

// Promote mints a community invite code for the guest. The host UI
// surfaces this as the "convert guest to member" affordance after
// rapport is established. Requires the lobby to carry an email so the
// caller can hand the resulting code to the guest cleanly.
func (s *Service) Promote(ctx context.Context, lobbyID string) (string, error) {
	l, err := s.Repo.ByID(ctx, lobbyID)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(l.GuestEmail) == "" {
		return "", ErrPromoteNeedsEmail
	}
	one := 1
	code, err := s.Invites.IssueInvite(ctx, l.CommunityID, &l.HostUserID, &one)
	if err != nil {
		return "", fmt.Errorf("issue invite: %w", err)
	}
	return code, nil
}

// randomToken returns a URL-safe random string of approximately
// `bytes` raw bytes of entropy (base64-url encoded → ~1.33x length).
func randomToken(b int) (string, error) {
	buf := make([]byte, b)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
