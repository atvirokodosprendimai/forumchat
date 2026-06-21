package chatagents_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chatagents"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
)

func TestThreadRunnerStreamsReplyToDone(t *testing.T) {
	ctx := context.Background()
	db := openMigrated(t)
	c, _ := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	u := auth.User{ID: uuid.NewString(), Email: "asker@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(db).CreateUser(ctx, u); err != nil {
		t.Fatalf("user: %v", err)
	}

	srv := stubOllama(t, "run ", "make deploy.")
	a := agent.Agent{
		ID: uuid.NewString(), CommunityID: c.ID, Name: "nick", Provider: "ollama",
		BaseURL: srv.URL, Model: "m", Enabled: true, InChatEnabled: true,
	}
	if err := agent.NewRepo(db).CreateAgent(ctx, a); err != nil {
		t.Fatalf("agent: %v", err) // FK target for thread.agent_id + post.agent_id
	}

	fRepo := forum.NewRepo(db)
	fSvc := forum.NewService(fRepo, time.Minute)
	agentID := a.ID
	th, err := fSvc.CreateThread(ctx, forum.CreateThreadInput{
		CommunityID: c.ID, AuthorID: u.ID, AgentID: &agentID,
		Subject: "how do I deploy?", BodyMarkdown: "@nick how do I deploy?",
	})
	if err != nil {
		t.Fatalf("create thread: %v", err)
	}

	runner := chatagents.NewThreadRunner(fRepo, forum.NewBus(), nil, 0, discard())
	runner.Generate(c.ID, th.ID, a)

	deadline := time.Now().Add(3 * time.Second)
	for {
		posts, _ := fRepo.ListPosts(ctx, th.ID)
		var bot *forum.Post
		for i := range posts {
			if posts[i].IsBot() {
				bot = &posts[i]
				break
			}
		}
		if bot != nil && bot.GenStatus == forum.GenDone {
			if bot.BodyMarkdown != "run make deploy." || bot.BodyHTML == "" {
				t.Fatalf("body wrong: md=%q html=%q", bot.BodyMarkdown, bot.BodyHTML)
			}
			if bot.BotName != "nick" || bot.AgentID == nil || *bot.AgentID != a.ID {
				t.Fatalf("identity wrong: name=%q agent=%v", bot.BotName, bot.AgentID)
			}
			if bot.AuthorID != forum.AgentBotUserID {
				t.Fatalf("author = %q, want sentinel %q", bot.AuthorID, forum.AgentBotUserID)
			}
			break
		}
		if time.Now().After(deadline) {
			status := "<no bot post>"
			if bot != nil {
				status = bot.GenStatus
			}
			t.Fatalf("runner did not finish; bot status %q", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMarkBotPostsInterrupted(t *testing.T) {
	ctx := context.Background()
	db := openMigrated(t)
	c, _ := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	u := auth.User{ID: uuid.NewString(), Email: "a@x.test", PasswordHash: "x", Status: auth.StatusActive}
	_ = auth.NewRepo(db).CreateUser(ctx, u)
	a := agent.Agent{ID: uuid.NewString(), CommunityID: c.ID, Name: "nick", Enabled: true}
	if err := agent.NewRepo(db).CreateAgent(ctx, a); err != nil {
		t.Fatalf("agent: %v", err)
	}
	fRepo := forum.NewRepo(db)
	th, _ := forum.NewService(fRepo, time.Minute).CreateThread(ctx, forum.CreateThreadInput{
		CommunityID: c.ID, AuthorID: u.ID, Subject: "s", BodyMarkdown: "b",
	})
	aid := a.ID
	if err := fRepo.InsertBotPost(ctx, forum.Post{
		ID: uuid.NewString(), ThreadID: th.ID, AuthorID: forum.AgentBotUserID,
		AgentID: &aid, BotName: "nick", GenStatus: forum.GenGenerating, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert bot post: %v", err)
	}
	n, err := fRepo.MarkBotPostsInterrupted(ctx)
	if err != nil || n != 1 {
		t.Fatalf("sweep n=%d err=%v, want 1", n, err)
	}
}
