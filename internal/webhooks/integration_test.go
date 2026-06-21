package webhooks_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	"github.com/atvirokodosprendimai/forumchat/internal/webhooks"
)

// TestInboundVertical exercises the full inbound path at the data layer:
// create an inbound webhook, look it up by token, post a bot message into its
// channel via the chat service, and read it back as a KindWebhook message that
// carries the webhook's bot identity.
func TestInboundVertical(t *testing.T) {
	t.Parallel()
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

	whRepo := webhooks.NewRepo(db)
	whSvc := webhooks.NewService(whRepo)
	wh, err := whSvc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID,
		Direction:   webhooks.DirIn,
		Provider:    "github",
		Name:        "GitHub",
		AvatarURL:   "https://example.com/gh.png",
		ChannelID:   general.ID,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}
	if wh.Token == "" {
		t.Fatal("inbound webhook should mint a token")
	}

	got, err := whRepo.InboundByToken(ctx, wh.Token)
	if err != nil {
		t.Fatalf("InboundByToken: %v", err)
	}
	if got.ChannelID != general.ID || got.Name != "GitHub" {
		t.Fatalf("InboundByToken mismatch: %+v", got)
	}

	if _, err := chatSvc.PostBot(ctx, c.ID, general.ID, got.Name, got.AvatarURL, "**alice** pushed 1 commit"); err != nil {
		t.Fatalf("PostBot: %v", err)
	}

	msgs, err := chatRepo.Recent(ctx, general.ID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Kind != chat.KindWebhook {
		t.Fatalf("kind = %q, want webhook", m.Kind)
	}
	if m.AuthorID != nil {
		t.Fatalf("bot message must have no author_id, got %v", *m.AuthorID)
	}
	if m.BotName != "GitHub" || m.AuthorName != "GitHub" {
		t.Fatalf("bot name not carried: BotName=%q AuthorName=%q", m.BotName, m.AuthorName)
	}
	if m.BotAvatar != "https://example.com/gh.png" {
		t.Fatalf("bot avatar not carried: %q", m.BotAvatar)
	}
}

// TestInboundByTokenMiss confirms a bad / disabled token is not found.
func TestInboundByTokenMiss(t *testing.T) {
	t.Parallel()
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
	repo := webhooks.NewRepo(db)
	if _, err := repo.InboundByToken(ctx, "does-not-exist"); err != webhooks.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := repo.InboundByToken(ctx, ""); err != webhooks.ErrNotFound {
		t.Fatalf("empty token want ErrNotFound, got %v", err)
	}
}

// TestOutboundCreateValidation checks provider×direction and URL validation.
func TestOutboundCreateValidation(t *testing.T) {
	t.Parallel()
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	svc := webhooks.NewService(webhooks.NewRepo(db))

	// github is inbound-only → rejected for outbound.
	if _, err := svc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID, Direction: webhooks.DirOut, Provider: "github",
		Name: "x", TargetURL: "https://example.com",
	}); err != webhooks.ErrBadProvider {
		t.Fatalf("want ErrBadProvider, got %v", err)
	}

	// outbound without a valid URL → rejected.
	if _, err := svc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID, Direction: webhooks.DirOut, Provider: "slack",
		Name: "x", TargetURL: "not-a-url",
	}); err != webhooks.ErrTargetURL {
		t.Fatalf("want ErrTargetURL, got %v", err)
	}

	// valid outbound slack webhook → ok, no token.
	wh, err := svc.Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID, Direction: webhooks.DirOut, Provider: "slack",
		Name: "Slack relay", TargetURL: "https://hooks.slack.com/services/x",
	})
	if err != nil {
		t.Fatalf("valid outbound create: %v", err)
	}
	if wh.Token != "" {
		t.Fatal("outbound webhook must not have a token")
	}
}

// TestInboundMediaVertical exercises the inbound media data path: save an upload
// under a synthetic webhook owner, post it as a KindWebhook message with no
// body, and read the attachment back. This is what the multipart inbound handler
// drives (Matrix → forumchat image bridge).
func TestInboundMediaVertical(t *testing.T) {
	t.Parallel()
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

	// uploads.owner_id is FK'd to users; mint a real owner.
	owner := auth.User{ID: uuid.NewString(), Email: "wh@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(db).CreateUser(ctx, owner); err != nil {
		t.Fatalf("create user: %v", err)
	}
	store := uploads.NewStore(db, t.TempDir(), 1<<20, "sign-key")
	u, err := store.Save(ctx, owner.ID, c.ID, "text/plain", "note.txt", bytes.NewReader([]byte("hello bytes")))
	if err != nil {
		t.Fatalf("save upload: %v", err)
	}

	// Empty body + one attachment must be allowed (image-only post).
	m, err := chatSvc.PostBotWithAttachments(ctx, c.ID, general.ID, "Bridge", "", "", []string{u.ID})
	if err != nil {
		t.Fatalf("PostBotWithAttachments: %v", err)
	}
	if m.Kind != chat.KindWebhook {
		t.Fatalf("kind = %q, want webhook", m.Kind)
	}

	byMsg, err := chatRepo.AttachmentsForMessages(ctx, []string{m.ID})
	if err != nil {
		t.Fatalf("AttachmentsForMessages: %v", err)
	}
	if got := len(byMsg[m.ID]); got != 1 {
		t.Fatalf("attachments = %d, want 1", got)
	}
	if byMsg[m.ID][0].UploadID != u.ID {
		t.Fatalf("attachment upload id = %q, want %q", byMsg[m.ID][0].UploadID, u.ID)
	}

	// Empty body + no attachments must still be rejected.
	if _, err := chatSvc.PostBotWithAttachments(ctx, c.ID, general.ID, "Bridge", "", "", nil); err == nil {
		t.Fatal("empty body + no attachments should error")
	}
}

// onePxPNG is a minimal valid PNG so uploads.MIMEFromHeader sniffs image/png.
var onePxPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

// TestPostInboundMultipart drives the public /hooks/{token} endpoint with the
// exact multipart shape an external bridge sends (a `text` field plus a `file`
// part) and asserts the image is stored and linked to a KindWebhook message —
// the full inbound-media path through the HTTP handler, not just the data layer.
func TestPostInboundMultipart(t *testing.T) {
	t.Parallel()
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	chatRepo := chat.NewRepo(db)
	general, err := chatRepo.EnsureDefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	// uploads.owner_id is FK'd to users; the webhook's creator owns its media.
	owner := auth.User{ID: uuid.NewString(), Email: "wh@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := auth.NewRepo(db).CreateUser(ctx, owner); err != nil {
		t.Fatalf("create user: %v", err)
	}

	whRepo := webhooks.NewRepo(db)
	wh, err := webhooks.NewService(whRepo).Create(ctx, webhooks.CreateInput{
		CommunityID: c.ID, Direction: webhooks.DirIn, Provider: "generic",
		Name: "Bridge", ChannelID: general.ID, CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("create webhook: %v", err)
	}

	h := &webhooks.Handler{
		Repo:     whRepo,
		Chat:     chat.NewService(chatRepo),
		ChatRepo: chatRepo,
		Uploads:  uploads.NewStore(db, t.TempDir(), 1<<20, "sign-key"),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Build the multipart body: a `text` field then an image `file` part —
	// the shape an external bridge (Matrix → forumchat) posts.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("text", "saint"); err != nil {
		t.Fatalf("write text field: %v", err)
	}
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="Screenshot%202025-10-26%20at%2020.14.03.png"`)
	hdr.Set("Content-Type", "image/png")
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(onePxPNG); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close mw: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/hooks/"+wh.Token, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()

	router := chi.NewRouter()
	router.Post("/hooks/{token}", h.PostInbound)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	msgs, err := chatRepo.Recent(ctx, general.ID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Kind != chat.KindWebhook {
		t.Fatalf("kind = %q, want webhook", m.Kind)
	}
	if m.BodyMarkdown != "saint" {
		t.Fatalf("body = %q, want %q", m.BodyMarkdown, "saint")
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(m.Attachments))
	}
	if m.Attachments[0].MIME != "image/png" {
		t.Fatalf("attachment mime = %q, want image/png", m.Attachments[0].MIME)
	}
}
