package chat_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
)

// seedAgent inserts a real ai_agents row so a bot bubble's bot_agent_id FK
// (REFERENCES ai_agents(id)) is satisfied. Returns the agent id.
func seedAgent(t *testing.T, repo *chat.Repo, cid, id string) string {
	t.Helper()
	now := time.Now().Unix()
	if err := agent.NewRepo(repo.DB).CreateAgent(context.Background(), agent.Agent{
		ID: id, CommunityID: cid, Name: id, Provider: "ollama", Model: "test",
		TriggerMode: agent.TriggerModeAll, TriggerPrefix: ".", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed agent %s: %v", id, err)
	}
	return id
}

// insertBotBubble seeds a generating kind='bot' message and returns its id.
func insertBotBubble(t *testing.T, repo *chat.Repo, cid, channelID, agentID string, asHuman bool) string {
	t.Helper()
	id := uuid.NewString()
	if err := repo.Insert(context.Background(), chat.Message{
		ID:          id,
		CommunityID: cid,
		ChannelID:   channelID,
		Kind:        chat.KindBot,
		BotName:     "Botty",
		BotAgentID:  &agentID,
		BotAsHuman:  asHuman,
		GenStatus:   chat.GenGenerating,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatalf("insert bot bubble: %v", err)
	}
	return id
}

// TestUpdateBotBody_PersistsGrowthAndStatus checks the streaming write path the
// ChannelRunner uses: the body + lifecycle update, and that bot_as_human
// round-trips so the read model renders the bubble as a member.
func TestUpdateBotBody_PersistsGrowthAndStatus(t *testing.T) {
	t.Parallel()
	repo, _, cid, _ := chanTestEnv(t)
	ctx := context.Background()
	ch, err := repo.DefaultChannel(ctx, cid)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	seedAgent(t, repo, cid, "agent-1")
	id := insertBotBubble(t, repo, cid, ch.ID, "agent-1", true)

	if err := repo.UpdateBotBody(ctx, id, "hello **world**", "<p>hello</p>", chat.GenDone); err != nil {
		t.Fatalf("update bot body: %v", err)
	}
	got, err := repo.ByID(ctx, id)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if got.BodyMarkdown != "hello **world**" || got.GenStatus != chat.GenDone {
		t.Fatalf("body/status not persisted: body=%q status=%q", got.BodyMarkdown, got.GenStatus)
	}
	if !got.BotAsHuman {
		t.Fatal("bot_as_human did not round-trip — bubble would render with the AI badge")
	}
	if got.BotAgentID == nil || *got.BotAgentID != "agent-1" {
		t.Fatalf("bot_agent_id lost: %v", got.BotAgentID)
	}
}

// TestMarkBotGeneratingInterrupted_HealsStuckBubbles checks the boot sweep:
// every still-generating bubble flips to interrupted (partial kept), and a
// finished one is left alone.
func TestMarkBotGeneratingInterrupted_HealsStuckBubbles(t *testing.T) {
	t.Parallel()
	repo, _, cid, _ := chanTestEnv(t)
	ctx := context.Background()
	ch, err := repo.DefaultChannel(ctx, cid)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	seedAgent(t, repo, cid, "agent-1")
	seedAgent(t, repo, cid, "agent-2")
	stuck := insertBotBubble(t, repo, cid, ch.ID, "agent-1", false)
	done := insertBotBubble(t, repo, cid, ch.ID, "agent-2", false)
	if err := repo.UpdateBotBody(ctx, done, "all done", "<p>all done</p>", chat.GenDone); err != nil {
		t.Fatalf("finish done bubble: %v", err)
	}

	n, err := repo.MarkBotGeneratingInterrupted(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d bubbles, want 1 (only the stuck one)", n)
	}
	gotStuck, _ := repo.ByID(ctx, stuck)
	if gotStuck.GenStatus != chat.GenInterrupted {
		t.Fatalf("stuck bubble status = %q, want interrupted", gotStuck.GenStatus)
	}
	gotDone, _ := repo.ByID(ctx, done)
	if gotDone.GenStatus != chat.GenDone {
		t.Fatalf("finished bubble was wrongly swept: status = %q", gotDone.GenStatus)
	}
}
