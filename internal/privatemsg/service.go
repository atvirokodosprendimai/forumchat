package privatemsg

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
	ErrSelfMessage      = errors.New("cannot message yourself")
	ErrNotAMember       = errors.New("not a participant of this thread")
	ErrNotPending       = errors.New("thread is not pending")
	ErrNotAcceptedYet   = errors.New("thread not accepted")
	ErrThreadDeclined   = errors.New("thread was declined")
	ErrEmptyBody        = errors.New("body cannot be empty")
)

type Service struct {
	Repo *Repo
	Bus  *Bus
}

// CreateRequest persists a brand-new pending DM thread plus its first message.
// If a thread between the two users already exists, the first message is
// appended to that thread instead and the thread status is left as-is.
func (s *Service) CreateRequest(ctx context.Context, fromUser, toUser, body, sourceCommunity, sourceChatMessage string) (Thread, Message, error) {
	if fromUser == toUser {
		return Thread{}, Message{}, ErrSelfMessage
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return Thread{}, Message{}, ErrEmptyBody
	}

	now := time.Now().UTC()
	existing, ok, err := s.Repo.ThreadBetween(ctx, fromUser, toUser)
	if err != nil {
		return Thread{}, Message{}, err
	}

	var t Thread
	if ok {
		// A declined thread is reopened back to pending so the recipient sees the new request.
		t = existing
		if t.Status == StatusDeclined {
			if err := s.Repo.UpdateThreadStatus(ctx, t.ID, StatusPending); err != nil {
				return Thread{}, Message{}, err
			}
			t.Status = StatusPending
		}
	} else {
		t = Thread{
			ID:                  uuid.NewString(),
			InitiatorUserID:     fromUser,
			RecipientUserID:     toUser,
			Status:              StatusPending,
			SourceCommunityID:   sourceCommunity,
			SourceChatMessageID: sourceChatMessage,
			LastMessageAt:       now,
			CreatedAt:           now,
		}
		if err := s.Repo.CreateThread(ctx, t); err != nil {
			return Thread{}, Message{}, err
		}
	}

	m, err := s.appendMessage(ctx, t.ID, fromUser, body, now)
	if err != nil {
		return Thread{}, Message{}, err
	}
	if err := s.Repo.BumpThreadLastMessage(ctx, t.ID, now); err != nil {
		return Thread{}, Message{}, err
	}
	// Sender has now read everything they sent.
	_ = s.Repo.MarkRead(ctx, t.ID, fromUser, now)

	s.Bus.Notify(t.RecipientUserID)
	s.Bus.Notify(t.InitiatorUserID)
	return t, m, nil
}

// SendMessage appends a new message to an accepted thread.
func (s *Service) SendMessage(ctx context.Context, threadID, fromUser, body string) (Message, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Message{}, ErrEmptyBody
	}
	t, err := s.Repo.ThreadByID(ctx, threadID)
	if err != nil {
		return Message{}, err
	}
	if !t.HasMember(fromUser) {
		return Message{}, ErrNotAMember
	}
	if t.Status == StatusDeclined {
		return Message{}, ErrThreadDeclined
	}
	if t.Status != StatusAccepted {
		// Only the original requester can append while still pending; recipient must Accept first.
		if fromUser != t.InitiatorUserID {
			return Message{}, ErrNotAcceptedYet
		}
	}
	now := time.Now().UTC()
	m, err := s.appendMessage(ctx, t.ID, fromUser, body, now)
	if err != nil {
		return Message{}, err
	}
	if err := s.Repo.BumpThreadLastMessage(ctx, t.ID, now); err != nil {
		return Message{}, err
	}
	_ = s.Repo.MarkRead(ctx, t.ID, fromUser, now)

	s.Bus.Notify(t.RecipientUserID)
	s.Bus.Notify(t.InitiatorUserID)
	return m, nil
}

// Accept marks a pending thread accepted. Only the recipient can call this.
func (s *Service) Accept(ctx context.Context, threadID, byUser string) error {
	t, err := s.Repo.ThreadByID(ctx, threadID)
	if err != nil {
		return err
	}
	if t.RecipientUserID != byUser {
		return ErrNotAMember
	}
	if t.Status != StatusPending {
		return ErrNotPending
	}
	if err := s.Repo.UpdateThreadStatus(ctx, t.ID, StatusAccepted); err != nil {
		return err
	}
	s.Bus.Notify(t.RecipientUserID)
	s.Bus.Notify(t.InitiatorUserID)
	return nil
}

// Decline marks a pending thread declined. Only the recipient can call this.
func (s *Service) Decline(ctx context.Context, threadID, byUser string) error {
	t, err := s.Repo.ThreadByID(ctx, threadID)
	if err != nil {
		return err
	}
	if t.RecipientUserID != byUser {
		return ErrNotAMember
	}
	if t.Status != StatusPending {
		return ErrNotPending
	}
	if err := s.Repo.UpdateThreadStatus(ctx, t.ID, StatusDeclined); err != nil {
		return err
	}
	s.Bus.Notify(t.RecipientUserID)
	s.Bus.Notify(t.InitiatorUserID)
	return nil
}

// MarkRead records that `byUser` has seen this thread up to `now`.
func (s *Service) MarkRead(ctx context.Context, threadID, byUser string) error {
	t, err := s.Repo.ThreadByID(ctx, threadID)
	if err != nil {
		return err
	}
	if !t.HasMember(byUser) {
		return ErrNotAMember
	}
	if err := s.Repo.MarkRead(ctx, threadID, byUser, time.Now().UTC()); err != nil {
		return err
	}
	s.Bus.Notify(byUser)
	return nil
}

func (s *Service) appendMessage(ctx context.Context, threadID, author, body string, now time.Time) (Message, error) {
	html, err := render.RenderMarkdown(body)
	if err != nil {
		return Message{}, fmt.Errorf("render: %w", err)
	}
	m := Message{
		ID:           uuid.NewString(),
		ThreadID:     threadID,
		AuthorUserID: author,
		Body:         body,
		BodyHTML:     html,
		CreatedAt:    now,
	}
	if err := s.Repo.CreateMessage(ctx, m); err != nil {
		return Message{}, err
	}
	return m, nil
}
