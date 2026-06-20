package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/google/uuid"
)

// Service is the write-side orchestration: minting threads and turns, rendering
// markdown at write time, and assembling the model history. It never talks to
// the Bus/NATS — broadcasting is the runner's and handler's job — so it stays
// trivially testable.
type Service struct{ Repo *Repo }

func NewService(repo *Repo) *Service { return &Service{Repo: repo} }

// CreateThread mints an empty conversation. visibility is normalised to
// private unless explicitly "shared".
func (s *Service) CreateThread(ctx context.Context, communityID, userID, visibility, model string) (Thread, error) {
	vis := VisibilityPrivate
	if visibility == VisibilityShared {
		vis = VisibilityShared
	}
	now := nowUnix()
	t := Thread{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		UserID:      userID,
		Visibility:  vis,
		Title:       "New chat",
		Model:       model,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.Repo.CreateThread(ctx, t); err != nil {
		return Thread{}, err
	}
	return t, nil
}

// Send persists a member's user turn plus an empty assistant placeholder
// (status=generating) and returns the placeholder id together with the model
// history the runner should generate against. The caller starts the runner.
func (s *Service) Send(ctx context.Context, t Thread, authorID, body string) (assistantID string, history []ChatMessage, err error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", nil, ErrEmptyBody
	}
	userHTML, err := render.RenderMarkdown(body)
	if err != nil {
		return "", nil, fmt.Errorf("render user markdown: %w", err)
	}
	now := nowUnix()
	userMsg := Message{
		ID: uuid.NewString(), ThreadID: t.ID, Role: RoleUser, AuthorID: authorID,
		BodyMD: body, BodyHTML: userHTML, Status: StatusDone, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Repo.InsertMessage(ctx, userMsg); err != nil {
		return "", nil, err
	}
	asst := Message{
		ID: uuid.NewString(), ThreadID: t.ID, Role: RoleAssistant,
		Status: StatusGenerating, CreatedAt: now + 1, UpdatedAt: now + 1,
	}
	if err := s.Repo.InsertMessage(ctx, asst); err != nil {
		return "", nil, err
	}
	_ = s.Repo.TouchThread(ctx, t.ID)
	if t.Title == "" || t.Title == "New chat" {
		if title := autoTitle(body); title != "" {
			_ = s.Repo.SetThreadTitle(ctx, t.ID, title)
		}
	}

	msgs, err := s.Repo.Messages(ctx, t.ID)
	if err != nil {
		return "", nil, err
	}
	return asst.ID, buildHistory(msgs), nil
}

// Regenerate resets the thread's last assistant turn back to an empty
// generating state and returns its id + the history up to (excluding) it, so
// the runner can stream a fresh answer. Used after an interrupted/errored turn.
func (s *Service) Regenerate(ctx context.Context, threadID string) (assistantID string, history []ChatMessage, err error) {
	msgs, err := s.Repo.Messages(ctx, threadID)
	if err != nil {
		return "", nil, err
	}
	lastIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			lastIdx = i
			break
		}
	}
	if lastIdx == -1 {
		return "", nil, ErrNotFound
	}
	target := msgs[lastIdx]
	if err := s.Repo.UpdateAssistantBody(ctx, target.ID, "", "", StatusGenerating, ""); err != nil {
		return "", nil, err
	}
	_ = s.Repo.TouchThread(ctx, threadID)
	return target.ID, buildHistory(msgs[:lastIdx]), nil
}

// buildHistory maps stored turns into the provider's message list: user/system
// turns verbatim, assistant turns only when cleanly completed (a half-streamed
// or errored answer is poor context). Empty bodies are dropped.
func buildHistory(msgs []Message) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleUser, RoleSystem:
			if strings.TrimSpace(m.BodyMD) == "" {
				continue
			}
			out = append(out, ChatMessage{Role: m.Role, Content: m.BodyMD})
		case RoleAssistant:
			if m.Status != StatusDone || strings.TrimSpace(m.BodyMD) == "" {
				continue
			}
			out = append(out, ChatMessage{Role: RoleAssistant, Content: m.BodyMD})
		}
	}
	return out
}

// autoTitle derives a short thread title from the first user prompt.
func autoTitle(body string) string {
	t := strings.TrimSpace(render.AutoTitle(body))
	if t == "" {
		t = strings.TrimSpace(body)
	}
	t = strings.Join(strings.Fields(t), " ")
	const max = 60
	if len(t) > max {
		t = strings.TrimSpace(t[:max]) + "…"
	}
	return t
}
