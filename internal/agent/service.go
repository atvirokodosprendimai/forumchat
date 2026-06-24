package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/google/uuid"
)

// Service is the write-side orchestration: minting agents, threads and turns,
// rendering markdown at write time, and assembling the model history. It never
// talks to the Bus/NATS — broadcasting is the runner's and handler's job — so
// it stays trivially testable.
//
// Resolve routes the synchronous /summary generation onto platform compute
// (metered) for an opted-in community, mirroring Runner.Resolve. Wired in
// main.go (SaaS); nil → the agent's BYO provider, unchanged.
type Service struct {
	Repo    *Repo
	Resolve ComputeResolver
}

func NewService(repo *Repo) *Service { return &Service{Repo: repo} }

// SaveAgent creates (when a.ID == "") or updates an agent, validating + caps.
func (s *Service) SaveAgent(ctx context.Context, a Agent) (Agent, error) {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return Agent{}, ErrNoName
	}
	a.Provider = strings.TrimSpace(a.Provider)
	if a.Provider == "" {
		a.Provider = ProviderOllama
	}
	a.BaseURL = strings.TrimSpace(a.BaseURL)
	a.Model = strings.TrimSpace(a.Model)
	now := nowUnix()
	if a.ID == "" {
		n, err := s.Repo.CountAgents(ctx, a.CommunityID)
		if err != nil {
			return Agent{}, err
		}
		if n >= MaxAgentsPerCommunity {
			return Agent{}, ErrAgentCap
		}
		a.ID = uuid.NewString()
		a.Position = n
		a.CreatedAt = now
		a.UpdatedAt = now
		if err := s.Repo.CreateAgent(ctx, a); err != nil {
			return Agent{}, err
		}
		if a.IsSummarizer {
			if err := s.Repo.ClearOtherSummarizers(ctx, a.CommunityID, a.ID); err != nil {
				return Agent{}, err
			}
		}
		return a, nil
	}
	a.UpdatedAt = now
	if err := s.Repo.UpdateAgent(ctx, a); err != nil {
		return Agent{}, err
	}
	if a.IsSummarizer {
		if err := s.Repo.ClearOtherSummarizers(ctx, a.CommunityID, a.ID); err != nil {
			return Agent{}, err
		}
	}
	return a, nil
}

// CreateThread mints an empty conversation pinned to agent a.
func (s *Service) CreateThread(ctx context.Context, communityID, userID string, a Agent, visibility string) (Thread, error) {
	vis := VisibilityPrivate
	if visibility == VisibilityShared {
		vis = VisibilityShared
	}
	now := nowUnix()
	t := Thread{
		ID:          uuid.NewString(),
		CommunityID: communityID,
		UserID:      userID,
		AgentID:     a.ID,
		Visibility:  vis,
		Title:       "New chat",
		Model:       a.Model,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.Repo.CreateThread(ctx, t); err != nil {
		return Thread{}, err
	}
	return t, nil
}

// Send persists a member's user turn (with optional base64 images for a vision
// agent) plus an empty assistant placeholder (status=generating) and returns
// the placeholder id together with the model history the runner generates against.
func (s *Service) Send(ctx context.Context, t Thread, authorID, body string, images []string) (assistantID string, history []ChatMessage, err error) {
	body = strings.TrimSpace(body)
	if body == "" && len(images) == 0 {
		return "", nil, ErrEmptyBody
	}
	userHTML, err := render.RenderMarkdown(body)
	if err != nil {
		return "", nil, fmt.Errorf("render user markdown: %w", err)
	}
	now := nowUnix()
	userMsg := Message{
		ID: uuid.NewString(), ThreadID: t.ID, Role: RoleUser, AuthorID: authorID,
		BodyMD: body, BodyHTML: userHTML, Status: StatusDone, Images: images,
		CreatedAt: now, UpdatedAt: now,
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
// generating state and returns its id + the history up to (excluding) it.
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
	if err := s.Repo.UpdateAssistantBody(ctx, target.ID, "", "", StatusGenerating, "", ""); err != nil {
		return "", nil, err
	}
	_ = s.Repo.TouchThread(ctx, threadID)
	return target.ID, buildHistory(msgs[:lastIdx]), nil
}

// SummarizeToThread creates a SHARED agent thread seeded with prompt, runs the
// agent to completion SYNCHRONOUSLY (no streaming runner), stores the answer,
// and returns the thread id + answer text. Used by the chat /summary slash
// command — the caller shows the answer in a panel and may post it. images is the
// set of base64-encoded image payloads the caller collected from the channel
// for a vision agent; pass nil for a text-only summary (the runner strips them
// from a non-vision agent's request anyway, but the caller already gates on
// a.Vision before reading the files).
func (s *Service) SummarizeToThread(ctx context.Context, communityID, userID string, a Agent, title, prompt string, images []string) (threadID, answer string, err error) {
	now := nowUnix()
	// Resolve compute first — on the platform branch a's provider/host/model is
	// overridden (the summarizer routes to the platform VISION model so an
	// image-bearing channel summary is understood) and prov is metered.
	prov, a, err := resolveProvider(ctx, s.Resolve, communityID, a)
	if err != nil {
		return "", "", err
	}
	t := Thread{
		ID: uuid.NewString(), CommunityID: communityID, UserID: userID, AgentID: a.ID,
		Visibility: VisibilityShared, Title: title, Model: a.Model, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Repo.CreateThread(ctx, t); err != nil {
		return "", "", err
	}
	userHTML, _ := render.RenderMarkdown(prompt)
	if err := s.Repo.InsertMessage(ctx, Message{
		ID: uuid.NewString(), ThreadID: t.ID, Role: RoleUser, AuthorID: userID,
		BodyMD: prompt, BodyHTML: userHTML, Status: StatusDone, Images: images, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		return "", "", err
	}

	if !a.Vision {
		images = nil // a text-only model 400s on image input
	}
	msgs := make([]ChatMessage, 0, 2)
	if sp := strings.TrimSpace(a.SystemPrompt); sp != "" {
		msgs = append(msgs, ChatMessage{Role: RoleSystem, Content: sp})
	}
	msgs = append(msgs, ChatMessage{Role: RoleUser, Content: prompt, Images: images})

	var sb strings.Builder
	if _, err := prov.Stream(ctx, a.Model, msgs, nil, func(d string) error {
		sb.WriteString(d)
		return ctx.Err()
	}); err != nil {
		return "", "", err
	}
	answer = strings.TrimSpace(sb.String())

	asstHTML, _ := render.RenderMarkdown(answer)
	if err := s.Repo.InsertMessage(ctx, Message{
		ID: uuid.NewString(), ThreadID: t.ID, Role: RoleAssistant,
		BodyMD: answer, BodyHTML: asstHTML, Status: StatusDone, CreatedAt: now + 1, UpdatedAt: now + 1,
	}); err != nil {
		return "", "", err
	}
	_ = s.Repo.TouchThread(ctx, t.ID)
	return t.ID, answer, nil
}

// buildHistory maps stored turns into the provider's message list: user/system
// turns verbatim (carrying any attached images), assistant turns only when
// cleanly completed (a half-streamed or errored answer is poor context).
func buildHistory(msgs []Message) []ChatMessage {
	out := make([]ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleUser, RoleSystem:
			if strings.TrimSpace(m.BodyMD) == "" && len(m.Images) == 0 {
				continue
			}
			out = append(out, ChatMessage{Role: m.Role, Content: m.BodyMD, Images: m.Images})
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
	if t == "" {
		t = "Image"
	}
	return t
}
