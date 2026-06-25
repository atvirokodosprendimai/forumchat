package chat_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func setupMentionHandler(t *testing.T) (*chat.Handler, string, auth.Identity) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cRepo := community.NewRepo(db)
	c, err := cRepo.BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	aRepo := auth.NewRepo(db)
	// seed two members
	for _, name := range []string{"Alice", "Albert", "Bob"} {
		u := auth.User{ID: uuid.NewString(), Email: strings.ToLower(name) + "@x.test", PasswordHash: "x", Status: auth.StatusActive}
		if err := aRepo.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		m := auth.Membership{ID: uuid.NewString(), UserID: u.ID, CommunityID: c.ID, DisplayName: name, Role: auth.RoleMember}
		if err := aRepo.CreateMembership(ctx, nil, m); err != nil {
			t.Fatalf("create membership: %v", err)
		}
	}
	// caller identity = a fresh user not in the community list, doesn't matter for read-only.
	callerUser := auth.User{ID: uuid.NewString(), Email: "caller@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := aRepo.CreateUser(ctx, callerUser); err != nil {
		t.Fatalf("create caller: %v", err)
	}
	callerMembership := auth.Membership{ID: uuid.NewString(), UserID: callerUser.ID, CommunityID: c.ID, DisplayName: "Caller", Role: auth.RoleMember}
	if err := aRepo.CreateMembership(ctx, nil, callerMembership); err != nil {
		t.Fatalf("create caller membership: %v", err)
	}
	// BootstrapOrFetch doesn't seed #general (main.go does that on boot);
	// the test fixture must, so channel-scoped reads/writes have a home.
	if _, err := chat.NewRepo(db).EnsureDefaultChannel(ctx, c.ID); err != nil {
		t.Fatalf("ensure default channel: %v", err)
	}
	h := &chat.Handler{
		Repo:          chat.NewRepo(db),
		AuthRepo:      aRepo,
		CommunityID:   c.ID,
		CommunityName: c.Name,
		Log:           slog.Default(),
	}
	return h, c.ID, auth.Identity{User: callerUser, Membership: callerMembership}
}

// TestChannelScope_InsertRecent asserts messages are scoped by channel:
// a message inserted into #general is returned by Recent(general) but not
// by Recent(some-other-channel-id). Also exercises the Insert default-
// channel fallback (empty ChannelID → #general).
func TestChannelScope_InsertRecent(t *testing.T) {
	t.Parallel()
	h, cid, _ := setupMentionHandler(t)
	ctx := context.Background()

	general, err := h.Repo.DefaultChannel(ctx, cid)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	if general.Slug != "general" || !general.IsDefault {
		t.Fatalf("want default #general, got %+v", general)
	}

	// Explicit channel id.
	if err := h.Repo.Insert(ctx, chat.Message{
		ID: uuid.NewString(), CommunityID: cid, ChannelID: general.ID,
		Kind: chat.KindUser, BodyMarkdown: "hi", BodyHTML: "hi", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert explicit: %v", err)
	}
	// Empty channel id → falls back to #general.
	if err := h.Repo.Insert(ctx, chat.Message{
		ID: uuid.NewString(), CommunityID: cid,
		Kind: chat.KindSystem, BodyMarkdown: "sys", BodyHTML: "sys", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert fallback: %v", err)
	}

	got, err := h.Repo.Recent(ctx, general.ID, 100)
	if err != nil {
		t.Fatalf("recent general: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages in #general, got %d", len(got))
	}

	other, err := h.Repo.Recent(ctx, "no-such-channel", 100)
	if err != nil {
		t.Fatalf("recent other: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("want 0 messages in unknown channel, got %d", len(other))
	}
}

// TestRecent_HidesSoftDeleted asserts the channel history loader excludes
// soft-deleted messages entirely — the query drops them, not the template, so
// EVERY viewer (member, mod, super-admin) sees the message vanish, never a
// "[message removed]" placeholder. A surviving reply whose parent was deleted
// must also not leak the parent's content through its quote snippet (the
// reply-parent JOIN filters deleted_at).
func TestRecent_HidesSoftDeleted(t *testing.T) {
	t.Parallel()
	h, cid, _ := setupMentionHandler(t)
	ctx := context.Background()

	general, err := h.Repo.DefaultChannel(ctx, cid)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}

	liveID, delID, replyID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if err := h.Repo.Insert(ctx, chat.Message{
		ID: liveID, CommunityID: cid, ChannelID: general.ID,
		Kind: chat.KindUser, BodyMarkdown: "still here", BodyHTML: "still here", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	if err := h.Repo.Insert(ctx, chat.Message{
		ID: delID, CommunityID: cid, ChannelID: general.ID,
		Kind: chat.KindUser, BodyMarkdown: "secret to remove", BodyHTML: "secret to remove", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert to-delete: %v", err)
	}
	// A reply pointing at the message we're about to delete.
	if err := h.Repo.Insert(ctx, chat.Message{
		ID: replyID, CommunityID: cid, ChannelID: general.ID, ReplyToID: &delID,
		Kind: chat.KindUser, BodyMarkdown: "re: secret", BodyHTML: "re: secret", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("insert reply: %v", err)
	}

	if err := h.Repo.SoftDelete(ctx, delID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	got, err := h.Repo.Recent(ctx, general.ID, 100)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	var sawReply bool
	for _, m := range got {
		if m.ID == delID {
			t.Fatalf("Recent must hide soft-deleted messages from channel history")
		}
		if m.ID == replyID {
			sawReply = true
			if m.ReplyTo != nil {
				t.Fatalf("reply to a deleted message must not leak the parent snippet, got %q", m.ReplyTo.Snippet)
			}
		}
	}
	if !sawReply {
		t.Fatalf("the surviving reply should still be in history")
	}
	if len(got) != 2 { // live + reply; deleted gone
		t.Fatalf("want 2 live messages, got %d", len(got))
	}
}

func TestGetMentionSearch_PrefixHits(t *testing.T) {
	t.Parallel()
	h, cid, id := setupMentionHandler(t)

	req := httptest.NewRequest(http.MethodGet, `/c/test/chat/mention?datastar=%7B%22mention_query%22%3A%22al%22%7D`, nil)
	ctx := community.WithContext(req.Context(), community.Community{ID: cid, Slug: "test", Name: "Test"})
	ctx = auth.WithIdentity(ctx, id)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	h.GetMentionSearch(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "Alice") {
		t.Errorf("want Alice in SSE body, got:\n%s", body)
	}
	if !strings.Contains(body, "Albert") {
		t.Errorf("want Albert in SSE body, got:\n%s", body)
	}
	if strings.Contains(body, "Bob") {
		t.Errorf("did not want Bob (prefix mismatch), got:\n%s", body)
	}
	if !strings.Contains(body, `id="mention-popup"`) {
		t.Errorf("want #mention-popup root, got:\n%s", body)
	}
}

func TestGetMentionSearch_EmptyQueryEmptyPopup(t *testing.T) {
	t.Parallel()
	h, cid, id := setupMentionHandler(t)

	req := httptest.NewRequest(http.MethodGet, `/c/test/chat/mention?datastar=%7B%22mention_query%22%3A%22%22%7D`, nil)
	ctx := community.WithContext(req.Context(), community.Community{ID: cid, Slug: "test", Name: "Test"})
	ctx = auth.WithIdentity(ctx, id)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	h.GetMentionSearch(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Alice") || strings.Contains(body, "Albert") || strings.Contains(body, "Bob") {
		t.Errorf("expected empty popup for empty query, got:\n%s", body)
	}
}

func TestGetMentionSearch_Unauthed(t *testing.T) {
	t.Parallel()
	h, cid, _ := setupMentionHandler(t)
	req := httptest.NewRequest(http.MethodGet, `/c/test/chat/mention?datastar=%7B%22mention_query%22%3A%22al%22%7D`, nil)
	ctx := community.WithContext(req.Context(), community.Community{ID: cid, Slug: "test", Name: "Test"})
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	h.GetMentionSearch(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}
