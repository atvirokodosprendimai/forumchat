package rooms

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// setupReset builds a service backed by a tmpdir DB with one seeded community
// and its rooms, and returns the service plus the first room's id.
func setupReset(t *testing.T) (*Service, string, string) {
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
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test Community")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	repo := NewRepo(db)
	if err := repo.EnsureSeeded(ctx, c.ID); err != nil {
		t.Fatalf("seed rooms: %v", err)
	}
	svc := NewService(repo, NewBus(), NewState())
	return svc, fmt.Sprintf("%s:room-01", c.ID), c.ID
}

// dirtyRoom puts a room into a non-default state: public, with an active
// invite and a guest chat message, then returns the seeded guest's identity.
func dirtyRoom(t *testing.T, svc *Service, roomID, cid string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := svc.Repo.SetPublic(ctx, roomID, true, now); err != nil {
		t.Fatalf("set public: %v", err)
	}
	if err := svc.Repo.CreateInvite(ctx, Invite{
		Token: "tok-" + roomID, RoomID: roomID, CreatedAt: now,
	}); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if err := svc.Repo.AppendChat(ctx, ChatMessage{
		ID: uuid.NewString(), RoomID: roomID, CommunityID: cid,
		AuthorName: "Guest", Body: "hi", BodyHTML: "<p>hi</p>", CreatedAt: now,
	}); err != nil {
		t.Fatalf("append chat: %v", err)
	}
}

// assertDefault checks the room is back in seeded state and the chat was
// moved (not dropped) into the archive table.
func assertDefault(t *testing.T, svc *Service, roomID string, wantArchived int) {
	t.Helper()
	ctx := context.Background()
	rm, err := svc.Repo.RoomByID(ctx, roomID)
	if err != nil {
		t.Fatalf("room by id: %v", err)
	}
	if rm.IsPublic {
		t.Error("room still public after reset")
	}
	if rm.AdminUserID != "" {
		t.Errorf("admin still set after reset: %q", rm.AdminUserID)
	}
	if _, err := svc.Repo.ActiveInviteForRoom(ctx, roomID); err != ErrNotFound {
		t.Errorf("invite still active after reset: %v", err)
	}
	live, err := svc.Repo.ListChat(ctx, roomID, 0)
	if err != nil {
		t.Fatalf("list chat: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("live chat not cleared: %d rows remain", len(live))
	}
	if got := archiveCount(t, svc.Repo.DB, roomID); got != wantArchived {
		t.Errorf("archived rows = %d, want %d", got, wantArchived)
	}
}

func archiveCount(t *testing.T, db *sql.DB, roomID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM room_chat_archive WHERE room_id = ?`, roomID,
	).Scan(&n); err != nil {
		t.Fatalf("archive count: %v", err)
	}
	return n
}

// Clean /leave by the last participant resets the room.
func TestReset_OnLastLeave(t *testing.T) {
	ctx := context.Background()
	svc, roomID, cid := setupReset(t)
	now := time.Now().UTC()
	dirtyRoom(t, svc, roomID, cid, now)

	g1 := Identity{GuestID: "g1", Name: "One"}
	g2 := Identity{GuestID: "g2", Name: "Two"}
	svc.State.Join(roomID, g1, true, true, now)
	svc.State.Join(roomID, g2, true, true, now)

	// First leaver does NOT reset — room still occupied.
	if err := svc.Leave(ctx, roomID, g1.Key()); err != nil {
		t.Fatalf("leave g1: %v", err)
	}
	if rm, _ := svc.Repo.RoomByID(ctx, roomID); !rm.IsPublic {
		t.Fatal("room reset too early — g2 is still present")
	}

	// Last leaver resets.
	if err := svc.Leave(ctx, roomID, g2.Key()); err != nil {
		t.Fatalf("leave g2: %v", err)
	}
	assertDefault(t, svc, roomID, 1)
}

// The janitor evicting the last stale member resets the room via the
// OnEmpty callback NewService wires to resetRoom.
func TestReset_OnJanitorEvict(t *testing.T) {
	svc, roomID, cid := setupReset(t)
	stale := time.Now().UTC().Add(-2 * staleAfter)
	dirtyRoom(t, svc, roomID, cid, time.Now().UTC())

	// Join with a stale last-seen so the sweep evicts immediately.
	svc.State.Join(roomID, Identity{GuestID: "g1", Name: "One"}, true, true, stale)

	changed, emptied := svc.State.SweepStale(time.Now().UTC())
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none (room emptied, not merely changed)", changed)
	}
	if len(emptied) != 1 || emptied[0] != roomID {
		t.Fatalf("emptied = %v, want [%s]", emptied, roomID)
	}
	// Fire the wired callback exactly as RunJanitor does.
	svc.State.emptyNotify(roomID)
	assertDefault(t, svc, roomID, 1)
}

// resetRoom on an already-default room is a harmless no-op.
func TestReset_Idempotent(t *testing.T) {
	svc, roomID, _ := setupReset(t)
	svc.resetRoom(context.Background(), roomID)
	assertDefault(t, svc, roomID, 0)
}
