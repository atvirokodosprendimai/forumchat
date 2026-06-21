package forum_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

type relayCall struct {
	author   string
	body     string
	threadID string
	postID   string
	subject  string
	root     bool
}

type promoteFixture struct {
	h        *forum.Handler
	chatRepo *chat.Repo
	chatSvc  *chat.Service
	forumDB  *forum.Repo
	cID      string
	channel  string
	userA    auth.User
	userB    auth.User
	relays   *[]relayCall
}

func setupPromote(t *testing.T) promoteFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	chatRepo := chat.NewRepo(db)
	chatSvc := chat.NewService(chatRepo)
	general, err := chatRepo.EnsureDefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	aRepo := auth.NewRepo(db)
	mkUser := func(email string) auth.User {
		u := auth.User{ID: uuid.NewString(), Email: email, PasswordHash: "x", Status: auth.StatusActive}
		if err := aRepo.CreateUser(ctx, u); err != nil {
			t.Fatalf("create user: %v", err)
		}
		return u
	}
	forumRepo := forum.NewRepo(db)
	relays := &[]relayCall{}
	h := &forum.Handler{
		Svc:         forum.NewService(forumRepo, 15*time.Minute),
		Repo:        forumRepo,
		Chat:        chatSvc,
		ChatRepo:    chatRepo,
		CommunityID: c.ID,
		BaseURL:     "http://localhost:8080",
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Capture outbound thread relays so tests can assert the webhook side
		// mirrors the same messages as our side.
		RelayThread: func(communityID, channelID, channelName, author, bodyMD, threadID, postID, subject string, root bool) {
			*relays = append(*relays, relayCall{
				author: author, body: bodyMD, threadID: threadID,
				postID: postID, subject: subject, root: root,
			})
		},
	}
	return promoteFixture{
		h: h, chatRepo: chatRepo, chatSvc: chatSvc, forumDB: forumRepo,
		cID: c.ID, channel: general.ID, userA: mkUser("a@x.test"), userB: mkUser("b@x.test"),
		relays: relays,
	}
}

func (f promoteFixture) send(t *testing.T, authorID, body string, replyTo *string) chat.Message {
	t.Helper()
	m, err := f.chatSvc.Send(context.Background(), chat.SendInput{
		CommunityID: f.cID, ChannelID: f.channel, AuthorID: authorID, BodyMarkdown: body, ReplyToID: replyTo,
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return m
}

// promote drives PostPromoteChat as the given user clicking "→ thread" on msgID.
func (f promoteFixture) promote(t *testing.T, promoter auth.User, msgID string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/c/test/forum/promote-chat?id="+msgID, nil)
	ctx := auth.WithIdentity(req.Context(), auth.Identity{
		User:       promoter,
		Membership: auth.Membership{UserID: promoter.ID, CommunityID: f.cID, Role: auth.RoleMember, DisplayName: promoter.Email},
	})
	rec := httptest.NewRecorder()
	f.h.PostPromoteChat(rec, req.WithContext(ctx))
	return rec.Code
}

// reply drives PostReply as the given user posting body into threadID.
func (f promoteFixture) reply(t *testing.T, replier auth.User, threadID, body string) int {
	t.Helper()
	payload := `{"body":` + strconv.Quote(body) + `,"quoted_post_id":"","image_data":""}`
	req := httptest.NewRequest(http.MethodPost, "/c/test/forum/"+threadID+"/reply", strings.NewReader(payload))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", threadID)
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	ctx = auth.WithIdentity(ctx, auth.Identity{
		User:       replier,
		Membership: auth.Membership{UserID: replier.ID, CommunityID: f.cID, Role: auth.RoleMember, DisplayName: replier.Email},
	})
	rec := httptest.NewRecorder()
	f.h.PostReply(rec, req.WithContext(ctx))
	return rec.Code
}

func (f promoteFixture) promotedThread(t *testing.T, msgID string) string {
	t.Helper()
	m, err := f.chatRepo.ByID(context.Background(), msgID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if m.PromotedThreadID == nil {
		t.Fatalf("message %s not promoted", msgID)
	}
	return *m.PromotedThreadID
}

// Promoting a reply pulls its parent in as the thread root and the reply itself
// becomes the thread's first post — both chat messages link to the new thread.
func TestPromoteReplyCarriesParent(t *testing.T) {
	f := setupPromote(t)
	a := f.send(t, f.userA.ID, "deploy plan?", nil)
	b := f.send(t, f.userB.ID, "ship friday", &a.ID)

	if code := f.promote(t, f.userB, b.ID); code != http.StatusOK {
		t.Fatalf("promote status = %d, want 200", code)
	}

	threadID := f.promotedThread(t, b.ID)
	if other := f.promotedThread(t, a.ID); other != threadID {
		t.Fatalf("parent linked to %s, reply to %s — want same thread", other, threadID)
	}

	th, err := f.forumDB.GetThread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.BodyMarkdown != "deploy plan?" {
		t.Fatalf("thread root body = %q, want the PARENT %q", th.BodyMarkdown, "deploy plan?")
	}

	posts, err := f.forumDB.ListPosts(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	if len(posts) != 1 {
		t.Fatalf("want 1 post (the reply), got %d", len(posts))
	}
	if posts[0].BodyMarkdown != "ship friday" {
		t.Fatalf("post body = %q, want the reply %q", posts[0].BodyMarkdown, "ship friday")
	}
	if posts[0].AuthorID != f.userB.ID {
		t.Fatalf("post author = %q, want reply author %q", posts[0].AuthorID, f.userB.ID)
	}
}

// The full scenario from the task: msg1, msg2 (a reply to msg1), promote msg2 →
// thread with root(msg1) + post(msg2); then msg3 replied in the thread. Our
// side has 3 messages and the webhook outgoing side must mirror all 3 — the
// announce (root = msg1), the promoted reply (msg2) and the thread reply (msg3).
// Regression: msg2 was created via Svc.CreatePost with no relay, so the webhook
// side dropped it and saw only 2 messages.
func TestPromoteReplyThreeMessagesRelayOutbound(t *testing.T) {
	f := setupPromote(t)
	a := f.send(t, f.userA.ID, "deploy plan?", nil)  // msg1
	b := f.send(t, f.userB.ID, "ship friday", &a.ID) // msg2 (reply to msg1)

	if code := f.promote(t, f.userB, b.ID); code != http.StatusOK {
		t.Fatalf("promote status = %d", code)
	}
	threadID := f.promotedThread(t, b.ID)

	if code := f.reply(t, f.userA, threadID, "what time?"); code != http.StatusOK { // msg3
		t.Fatalf("reply status = %d", code)
	}

	relays := *f.relays
	if len(relays) != 3 {
		t.Fatalf("want 3 outbound relays (announce + 2 posts), got %d: %+v", len(relays), relays)
	}
	// Exactly one root (the announce, carrying msg1's subject); the other two are
	// the promoted reply (msg2) and the thread reply (msg3), both tagged with the
	// thread id and a non-empty post id.
	var roots int
	bodies := map[string]bool{}
	for _, rc := range relays {
		if rc.root {
			roots++
			if rc.subject != "deploy plan?" {
				t.Errorf("announce subject = %q, want msg1 %q", rc.subject, "deploy plan?")
			}
			continue
		}
		if rc.threadID != threadID || rc.postID == "" {
			t.Errorf("post relay missing thread/post identity: %+v", rc)
		}
		bodies[rc.body] = true
	}
	if roots != 1 {
		t.Errorf("want exactly 1 root (announce) relay, got %d", roots)
	}
	if !bodies["ship friday"] {
		t.Errorf("webhook side dropped the promoted reply (msg2 'ship friday')")
	}
	if !bodies["what time?"] {
		t.Errorf("webhook side dropped the thread reply (msg3 'what time?')")
	}
}

// Promoting a non-reply message keeps the single-message behaviour: it is the
// root and the thread has no posts.
func TestPromoteStandaloneUnchanged(t *testing.T) {
	f := setupPromote(t)
	a := f.send(t, f.userA.ID, "standalone idea", nil)

	if code := f.promote(t, f.userA, a.ID); code != http.StatusOK {
		t.Fatalf("promote status = %d, want 200", code)
	}
	threadID := f.promotedThread(t, a.ID)
	th, err := f.forumDB.GetThread(context.Background(), threadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.BodyMarkdown != "standalone idea" {
		t.Fatalf("thread body = %q, want %q", th.BodyMarkdown, "standalone idea")
	}
	posts, err := f.forumDB.ListPosts(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	if len(posts) != 0 {
		t.Fatalf("standalone promote must have 0 posts, got %d", len(posts))
	}
}

// When the parent already opened a thread, promoting its reply folds the reply
// into that existing thread instead of spawning a duplicate.
func TestPromoteReplyFoldsIntoExistingThread(t *testing.T) {
	f := setupPromote(t)
	a := f.send(t, f.userA.ID, "deploy plan?", nil)
	b := f.send(t, f.userB.ID, "ship friday", &a.ID)

	// Promote the PARENT first → opens thread T with no posts.
	if code := f.promote(t, f.userA, a.ID); code != http.StatusOK {
		t.Fatalf("promote parent status = %d", code)
	}
	threadID := f.promotedThread(t, a.ID)

	// Now promote the reply → it must fold into T, not create a second thread.
	if code := f.promote(t, f.userB, b.ID); code != http.StatusOK {
		t.Fatalf("promote reply status = %d", code)
	}
	if got := f.promotedThread(t, b.ID); got != threadID {
		t.Fatalf("reply opened a new thread %s, want fold into %s", got, threadID)
	}
	posts, err := f.forumDB.ListPosts(context.Background(), threadID)
	if err != nil {
		t.Fatalf("ListPosts: %v", err)
	}
	if len(posts) != 1 || posts[0].BodyMarkdown != "ship friday" {
		t.Fatalf("want the reply folded as 1 post, got %d: %+v", len(posts), posts)
	}
}
