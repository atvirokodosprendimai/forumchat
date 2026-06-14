package lobbies_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/lobbies"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func setupRepo(t *testing.T) (*lobbies.Repo, string, string) {
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	aRepo := auth.NewRepo(db)
	host := auth.User{ID: uuid.NewString(), Email: "host@x.test", PasswordHash: "x", Status: auth.StatusActive}
	if err := aRepo.CreateUser(ctx, host); err != nil {
		t.Fatalf("create host: %v", err)
	}
	hostMembership := auth.Membership{ID: uuid.NewString(), UserID: host.ID, CommunityID: c.ID, DisplayName: "Host", Role: auth.RoleAdmin}
	if err := aRepo.CreateMembership(ctx, nil, hostMembership); err != nil {
		t.Fatalf("create host membership: %v", err)
	}
	return lobbies.NewRepo(db), c.ID, host.ID
}

func mintLobby(t *testing.T, repo *lobbies.Repo, communityID, hostID, token string) lobbies.Lobby {
	t.Helper()
	now := time.Now()
	l := lobbies.Lobby{
		ID:               uuid.NewString(),
		CommunityID:      communityID,
		HostUserID:       hostID,
		Medium:           lobbies.MediumLobby,
		GuestDisplayName: "Guest",
		GuestToken:       token,
		Status:           lobbies.StatusOpen,
		CreatedAt:        now,
		LastActivityAt:   now,
	}
	if err := repo.Create(context.Background(), l); err != nil {
		t.Fatalf("create lobby: %v", err)
	}
	return l
}

func TestCreateAndLookup_Roundtrip(t *testing.T) {
	t.Parallel()
	repo, cid, hostID := setupRepo(t)
	mintLobby(t, repo, cid, hostID, "tok-A")

	byID, err := repo.ByID(context.Background(), mustOne(t, repo, cid).ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if byID.GuestToken != "tok-A" {
		t.Fatalf("want tok-A, got %q", byID.GuestToken)
	}
	byTok, err := repo.ByToken(context.Background(), "tok-A")
	if err != nil {
		t.Fatalf("ByToken: %v", err)
	}
	if byTok.ID != byID.ID {
		t.Fatalf("token lookup mismatch")
	}
}

func TestByID_Missing(t *testing.T) {
	t.Parallel()
	repo, _, _ := setupRepo(t)
	if _, err := repo.ByID(context.Background(), "nope"); err == nil {
		t.Fatal("want ErrNotFound")
	}
}

func TestCreate_DuplicateTokenRejected(t *testing.T) {
	t.Parallel()
	repo, cid, hostID := setupRepo(t)
	mintLobby(t, repo, cid, hostID, "dup")
	err := repo.Create(context.Background(), lobbies.Lobby{
		ID: uuid.NewString(), CommunityID: cid, HostUserID: hostID,
		Medium: lobbies.MediumLobby, GuestToken: "dup",
		Status: lobbies.StatusOpen,
		CreatedAt: time.Now(), LastActivityAt: time.Now(),
	})
	if err != lobbies.ErrTokenTaken {
		t.Fatalf("want ErrTokenTaken, got %v", err)
	}
}

func TestListByCommunity_StatusFilter(t *testing.T) {
	t.Parallel()
	repo, cid, hostID := setupRepo(t)
	open := mintLobby(t, repo, cid, hostID, "open-1")
	archived := mintLobby(t, repo, cid, hostID, "arch-1")
	if err := repo.UpdateStatus(context.Background(), archived.ID, lobbies.StatusArchived); err != nil {
		t.Fatalf("archive: %v", err)
	}

	openList, err := repo.ListByCommunity(context.Background(), cid, lobbies.StatusOpen)
	if err != nil {
		t.Fatalf("list open: %v", err)
	}
	if len(openList) != 1 || openList[0].ID != open.ID {
		t.Fatalf("want only open lobby, got %+v", openList)
	}
	all, err := repo.ListByCommunity(context.Background(), cid, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("want 2 total, got %d", len(all))
	}
}

func TestAppendMessage_RecentMessagesOrdering(t *testing.T) {
	t.Parallel()
	repo, cid, hostID := setupRepo(t)
	l := mintLobby(t, repo, cid, hostID, "msg-tok")
	now := time.Now()
	for i, body := range []string{"first", "second", "third"} {
		if err := repo.AppendMessage(context.Background(), lobbies.LobbyMessage{
			ID:           uuid.NewString(),
			LobbyID:      l.ID,
			AuthorKind:   lobbies.AuthorHost,
			BodyMarkdown: body,
			BodyHTML:     "<p>" + body + "</p>",
			CreatedAt:    now.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	got, err := repo.RecentMessages(context.Background(), l.ID, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	if got[0].BodyMarkdown != "third" {
		t.Fatalf("want newest first; got %q", got[0].BodyMarkdown)
	}
}

func TestTouchActivity_BumpsTimestamp(t *testing.T) {
	t.Parallel()
	repo, cid, hostID := setupRepo(t)
	l := mintLobby(t, repo, cid, hostID, "touch")
	time.Sleep(1100 * time.Millisecond)
	if err := repo.TouchActivity(context.Background(), l.ID); err != nil {
		t.Fatalf("touch: %v", err)
	}
	fresh, _ := repo.ByID(context.Background(), l.ID)
	if !fresh.LastActivityAt.After(l.LastActivityAt) {
		t.Fatalf("want bumped; before=%v after=%v", l.LastActivityAt, fresh.LastActivityAt)
	}
}

func TestDelete_CascadesMessages(t *testing.T) {
	t.Parallel()
	repo, cid, hostID := setupRepo(t)
	l := mintLobby(t, repo, cid, hostID, "del")
	if err := repo.AppendMessage(context.Background(), lobbies.LobbyMessage{
		ID: uuid.NewString(), LobbyID: l.ID, AuthorKind: lobbies.AuthorHost,
		BodyMarkdown: "hi", BodyHTML: "<p>hi</p>", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := repo.Delete(context.Background(), l.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	msgs, err := repo.RecentMessages(context.Background(), l.ID, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected cascade, got %d messages", len(msgs))
	}
}

func mustOne(t *testing.T, repo *lobbies.Repo, cid string) lobbies.LobbyRow {
	t.Helper()
	rows, err := repo.ListByCommunity(context.Background(), cid, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 lobby, got %d", len(rows))
	}
	return rows[0]
}
