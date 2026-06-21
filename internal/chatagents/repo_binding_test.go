package chatagents_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
)

// TestAgentChannelBinding exercises the query layer behind the roster, mention
// autocomplete, and trigger dispatch: SetAgentChannels + AgentsForChannel +
// ListInChatAgents respect enabled / in_chat_enabled and the channel binding.
func TestAgentChannelBinding(t *testing.T) {
	ctx := context.Background()
	db := openMigrated(t)
	c, _ := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	u := auth.User{ID: uuid.NewString(), Email: "creator@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(db).CreateUser(ctx, u); err != nil {
		t.Fatalf("user: %v", err)
	}
	chatRepo := chat.NewRepo(db)
	general, _ := chatRepo.EnsureDefaultChannel(ctx, c.ID)
	dev, err := chat.NewService(chatRepo).CreateChannel(ctx, c.ID, u.ID, "dev", "")
	if err != nil {
		t.Fatalf("channel: %v", err)
	}

	aRepo := agent.NewRepo(db)
	// Enabled + in-chat agent bound to #dev only.
	nick := agent.Agent{ID: uuid.NewString(), CommunityID: c.ID, Name: "nick", Enabled: true, InChatEnabled: true}
	// Enabled but NOT in-chat — must never surface as a chat participant.
	mute := agent.Agent{ID: uuid.NewString(), CommunityID: c.ID, Name: "mute", Enabled: true, InChatEnabled: false}
	for _, a := range []agent.Agent{nick, mute} {
		if err := aRepo.CreateAgent(ctx, a); err != nil {
			t.Fatalf("create agent %s: %v", a.Name, err)
		}
	}
	if err := aRepo.SetAgentChannels(ctx, nick.ID, []string{dev.ID}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	// ListInChatAgents: only nick (in-chat enabled), regardless of channel.
	inChat, err := aRepo.ListInChatAgents(ctx, c.ID)
	if err != nil {
		t.Fatalf("list in-chat: %v", err)
	}
	if len(inChat) != 1 || inChat[0].ID != nick.ID {
		t.Fatalf("ListInChatAgents = %d agents, want only nick", len(inChat))
	}

	// AgentsForChannel(#dev) → nick; AgentsForChannel(#general) → none.
	if got, _ := aRepo.AgentsForChannel(ctx, c.ID, dev.ID); len(got) != 1 || got[0].ID != nick.ID {
		t.Fatalf("AgentsForChannel(dev) = %v, want [nick]", got)
	}
	if got, _ := aRepo.AgentsForChannel(ctx, c.ID, general.ID); len(got) != 0 {
		t.Fatalf("AgentsForChannel(general) = %d, want 0", len(got))
	}

	// Rebind to #general; #dev becomes empty.
	if err := aRepo.SetAgentChannels(ctx, nick.ID, []string{general.ID}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if got, _ := aRepo.AgentsForChannel(ctx, c.ID, general.ID); len(got) != 1 {
		t.Fatalf("after rebind AgentsForChannel(general) = %d, want 1", len(got))
	}
	if got, _ := aRepo.AgentsForChannel(ctx, c.ID, dev.ID); len(got) != 0 {
		t.Fatalf("after rebind AgentsForChannel(dev) = %d, want 0", len(got))
	}
	if ids, _ := aRepo.ChannelIDsForAgent(ctx, nick.ID); len(ids) != 1 || ids[0] != general.ID {
		t.Fatalf("ChannelIDsForAgent = %v, want [general]", ids)
	}
}
