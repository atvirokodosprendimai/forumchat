package chatagents_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/chatagents"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func openMigrated(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func stubOllama(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			io.WriteString(w, `{"message":{"role":"assistant","content":"`+c+`"},"done":false}`+"\n")
			if fl != nil {
				fl.Flush()
			}
		}
		io.WriteString(w, `{"message":{"role":"assistant","content":""},"done":true}`+"\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunnerStreamsBotBubbleToDone(t *testing.T) {
	ctx := context.Background()
	db := openMigrated(t)
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	chatRepo := chat.NewRepo(db)
	ch, err := chatRepo.EnsureDefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("channel: %v", err)
	}

	srv := stubOllama(t, "Hi ", "there.")
	a := agent.Agent{
		ID: uuid.NewString(), CommunityID: c.ID, Name: "nick", Provider: "ollama",
		BaseURL: srv.URL, Model: "m", Enabled: true, InChatEnabled: true,
		TriggerMode: agent.TriggerModeMention, TriggerPrefix: ".",
	}
	if err := agent.NewRepo(db).CreateAgent(ctx, a); err != nil {
		t.Fatalf("create agent: %v", err) // FK target for bot_agent_id
	}

	// Seed the triggering user message so buildHistory has channel context.
	if err := chatRepo.Insert(ctx, chat.Message{
		ID: uuid.NewString(), CommunityID: c.ID, ChannelID: ch.ID,
		Kind: chat.KindUser, BodyMarkdown: "@nick hello", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed user msg: %v", err)
	}

	runner := chatagents.NewRunner(chatRepo, chat.NewBus(), nil, 0, discard())
	runner.Trigger(c.ID, ch.ID, a)

	deadline := time.Now().Add(3 * time.Second)
	for {
		msgs, _ := chatRepo.Recent(ctx, ch.ID, 50)
		var bot *chat.Message
		for i := range msgs {
			if msgs[i].Kind == chat.KindBot {
				bot = &msgs[i]
				break
			}
		}
		if bot != nil && bot.GenStatus == chat.GenDone {
			if bot.BodyMarkdown != "Hi there." || bot.BodyHTML == "" {
				t.Fatalf("body wrong: md=%q html=%q", bot.BodyMarkdown, bot.BodyHTML)
			}
			if bot.BotName != "nick" || bot.BotAgentID == nil || *bot.BotAgentID != a.ID {
				t.Fatalf("bot identity wrong: name=%q agentID=%v", bot.BotName, bot.BotAgentID)
			}
			break
		}
		if time.Now().After(deadline) {
			status := "<no bot row>"
			if bot != nil {
				status = bot.GenStatus
			}
			t.Fatalf("runner did not finish; bot status %q", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMarkBotGeneratingInterrupted(t *testing.T) {
	ctx := context.Background()
	db := openMigrated(t)
	c, _ := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	chatRepo := chat.NewRepo(db)
	ch, _ := chatRepo.EnsureDefaultChannel(ctx, c.ID)

	id := uuid.NewString()
	if err := chatRepo.Insert(ctx, chat.Message{
		ID: id, CommunityID: c.ID, ChannelID: ch.ID, Kind: chat.KindBot,
		BotName: "nick", GenStatus: chat.GenGenerating, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert bot row: %v", err)
	}

	n, err := chatRepo.MarkBotGeneratingInterrupted(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("swept %d rows, want 1", n)
	}
	m, err := chatRepo.ByID(ctx, id)
	if err != nil {
		t.Fatalf("byid: %v", err)
	}
	if m.GenStatus != chat.GenInterrupted {
		t.Fatalf("gen_status = %q, want interrupted", m.GenStatus)
	}
}
