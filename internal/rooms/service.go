package rooms

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

var (
	ErrRoomFull       = errors.New("rooms: room is full")
	ErrNotAdmin       = errors.New("rooms: caller is not the room admin")
	ErrNotMember      = errors.New("rooms: caller is not in the room")
	ErrEmptyBody      = errors.New("rooms: empty body")
	ErrInvalidName    = errors.New("rooms: invalid room name")
	ErrInviteInactive = errors.New("rooms: invite link is no longer valid")
	ErrNoAdminYet     = errors.New("rooms: no admin available to approve guests")
)

type Service struct {
	Repo  *Repo
	Bus   *Bus
	State *State
}

func NewService(repo *Repo, bus *Bus, state *State) *Service {
	s := &Service{Repo: repo, Bus: bus, State: state}
	// Janitor calls this whenever it evicts stale members; we fan it out as
	// a room-wide presence event so every open SSE stream resyncs.
	state.OnChange(func(roomID string) {
		bus.PublishRoom(roomID, Event{Kind: "presence"})
	})
	return s
}

// JoinAuth admits a logged-in user. Returns the resulting JoinResult plus
// the snapshot post-write so handlers can render.
func (s *Service) JoinAuth(ctx context.Context, roomID, userID, displayName string) (JoinResult, error) {
	rm, err := s.Repo.RoomByID(ctx, roomID)
	if err != nil {
		return JoinResult{}, err
	}
	id := Identity{UserID: userID, Name: displayName}
	res := s.State.Join(roomID, id, rm.IsPublic, false, time.Now().UTC())
	if res.BecameAdmin {
		_ = s.Repo.SetAdmin(ctx, roomID, userID, time.Now().UTC())
	}
	if !res.Admitted && !res.Pending {
		switch res.Reason {
		case "full":
			return res, ErrRoomFull
		case "no_admin_yet":
			return res, ErrNoAdminYet
		}
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "presence"})
	return res, nil
}

// JoinGuest admits an invite-link guest. Validates the token and the room
// match, then admits straight to Members (invite acts like public).
func (s *Service) JoinGuest(ctx context.Context, token, displayName string) (Room, Identity, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return Room{}, Identity{}, ErrInvalidName
	}
	inv, err := s.Repo.InviteByToken(ctx, token)
	if err != nil {
		return Room{}, Identity{}, err
	}
	if !inv.Active(time.Now().UTC()) {
		return Room{}, Identity{}, ErrInviteInactive
	}
	rm, err := s.Repo.RoomByID(ctx, inv.RoomID)
	if err != nil {
		return Room{}, Identity{}, err
	}
	id := Identity{GuestID: uuid.NewString(), Name: displayName}
	res := s.State.Join(rm.ID, id, rm.IsPublic, true, time.Now().UTC())
	if !res.Admitted {
		if res.Reason == "full" {
			return Room{}, Identity{}, ErrRoomFull
		}
		return Room{}, Identity{}, fmt.Errorf("guest join failed: %s", res.Reason)
	}
	s.Bus.PublishRoom(rm.ID, Event{Kind: "presence"})
	return rm, id, nil
}

func (s *Service) Approve(ctx context.Context, roomID, byKey, targetKey string) error {
	if _, ok := s.State.Approve(roomID, byKey, targetKey); !ok {
		return ErrNotAdmin
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "approval"})
	s.Bus.PublishRoom(roomID, Event{Kind: "presence"})
	return nil
}

func (s *Service) Decline(ctx context.Context, roomID, byKey, targetKey string) error {
	if !s.State.Decline(roomID, byKey, targetKey) {
		return ErrNotAdmin
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "approval"})
	return nil
}

func (s *Service) Leave(ctx context.Context, roomID, key string) error {
	newAdmin, empty := s.State.Leave(roomID, key)
	if empty {
		_ = s.Repo.SetAdmin(ctx, roomID, "", time.Now().UTC())
	} else if newAdmin != "" {
		// newAdmin is a participant key — convert to user id for persistence.
		// auth keys are "u:<userID>"; guests can't be admin.
		if strings.HasPrefix(newAdmin, "u:") {
			_ = s.Repo.SetAdmin(ctx, roomID, strings.TrimPrefix(newAdmin, "u:"), time.Now().UTC())
		}
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "presence"})
	s.Bus.SendSignal(roomID, key, SignalEnvelope{Kind: "bye"}) // closes any open inbox
	return nil
}

func (s *Service) Promote(ctx context.Context, roomID, byKey, targetKey string) error {
	if !s.State.Promote(roomID, byKey, targetKey) {
		return ErrNotAdmin
	}
	if strings.HasPrefix(targetKey, "u:") {
		_ = s.Repo.SetAdmin(ctx, roomID, strings.TrimPrefix(targetKey, "u:"), time.Now().UTC())
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "presence"})
	return nil
}

func (s *Service) TogglePublic(ctx context.Context, roomID, byKey string) (bool, error) {
	if !s.State.IsAdmin(roomID, byKey) {
		return false, ErrNotAdmin
	}
	rm, err := s.Repo.RoomByID(ctx, roomID)
	if err != nil {
		return false, err
	}
	newVal := !rm.IsPublic
	if err := s.Repo.SetPublic(ctx, roomID, newVal, time.Now().UTC()); err != nil {
		return false, err
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "meta"})
	return newVal, nil
}

func (s *Service) Rename(ctx context.Context, roomID, byKey, name string) error {
	if !s.State.IsAdmin(roomID, byKey) {
		return ErrNotAdmin
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return ErrInvalidName
	}
	if err := s.Repo.UpdateRoomName(ctx, roomID, name, time.Now().UTC()); err != nil {
		return err
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "meta"})
	return nil
}

// CreateInvite issues a fresh long token, revoking any prior active one so
// only one share-link is "current" at a time.
func (s *Service) CreateInvite(ctx context.Context, roomID, byKey, byUserID string) (Invite, error) {
	if !s.State.IsAdmin(roomID, byKey) {
		return Invite{}, ErrNotAdmin
	}
	now := time.Now().UTC()
	if prev, err := s.Repo.ActiveInviteForRoom(ctx, roomID); err == nil {
		_ = s.Repo.RevokeInvite(ctx, prev.Token, now)
	}
	token, err := randToken(32)
	if err != nil {
		return Invite{}, err
	}
	inv := Invite{
		Token:     token,
		RoomID:    roomID,
		CreatedBy: byUserID,
		CreatedAt: now,
	}
	if err := s.Repo.CreateInvite(ctx, inv); err != nil {
		return Invite{}, err
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "meta"})
	return inv, nil
}

func (s *Service) RevokeInvite(ctx context.Context, roomID, byKey, token string) error {
	if !s.State.IsAdmin(roomID, byKey) {
		return ErrNotAdmin
	}
	inv, err := s.Repo.InviteByToken(ctx, token)
	if err != nil {
		return err
	}
	if inv.RoomID != roomID {
		return ErrNotAdmin
	}
	if err := s.Repo.RevokeInvite(ctx, token, time.Now().UTC()); err != nil {
		return err
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "meta"})
	return nil
}

// PostChat appends a chat message and notifies the room. Member-only.
func (s *Service) PostChat(ctx context.Context, roomID string, by Identity, body string) (ChatMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return ChatMessage{}, ErrEmptyBody
	}
	if !s.State.IsMember(roomID, by.Key()) {
		return ChatMessage{}, ErrNotMember
	}
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return ChatMessage{}, fmt.Errorf("render: %w", err)
	}
	m := ChatMessage{
		ID:           uuid.NewString(),
		RoomID:       roomID,
		AuthorUserID: by.UserID, // empty for guests — repo stores NULL
		AuthorName:   by.Name,
		Body:         body,
		BodyHTML:     html,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.Repo.AppendChat(ctx, m); err != nil {
		return ChatMessage{}, err
	}
	s.Bus.PublishRoom(roomID, Event{Kind: "chat"})
	return m, nil
}

func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
