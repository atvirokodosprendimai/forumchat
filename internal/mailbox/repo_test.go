package mailbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUpsertFolder_NewThenRotation(t *testing.T) {
	repo := setupTestRepo(t)
	ctx := context.Background()
	acc, err := repo.EnsureAccount(ctx, AccountConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p", TLSMode: "tls",
	})
	if err != nil {
		t.Fatalf("ensure account: %v", err)
	}

	// First sight — fresh row with last_uid=0 and the given uidvalidity.
	f1, err := repo.UpsertFolder(ctx, acc.ID, "INBOX", 1000)
	if err != nil {
		t.Fatalf("upsert new: %v", err)
	}
	if f1.LastUID != 0 || f1.UIDValidity != 1000 {
		t.Fatalf("expected fresh folder, got %+v", f1)
	}

	// Advance cursor and re-poll with same uidvalidity — last_uid persists.
	if err := repo.SetFolderLastUID(ctx, f1.ID, 42); err != nil {
		t.Fatalf("set last uid: %v", err)
	}
	f2, err := repo.UpsertFolder(ctx, acc.ID, "INBOX", 1000)
	if err != nil {
		t.Fatalf("upsert same uidvalidity: %v", err)
	}
	if f2.LastUID != 42 {
		t.Fatalf("cursor should survive same-uidvalidity poll, got %d", f2.LastUID)
	}

	// UIDVALIDITY rotation — cursor MUST reset.
	f3, err := repo.UpsertFolder(ctx, acc.ID, "INBOX", 2000)
	if err != nil {
		t.Fatalf("upsert rotated: %v", err)
	}
	if f3.LastUID != 0 || f3.UIDValidity != 2000 {
		t.Fatalf("rotation should reset cursor, got %+v", f3)
	}
}

func TestSetFolderLastUID_Monotonic(t *testing.T) {
	repo := setupTestRepo(t)
	ctx := context.Background()
	acc, _ := repo.EnsureAccount(ctx, AccountConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p", TLSMode: "tls",
	})
	f, _ := repo.UpsertFolder(ctx, acc.ID, "INBOX", 1000)

	if err := repo.SetFolderLastUID(ctx, f.ID, 5); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Lower UID should be ignored.
	if err := repo.SetFolderLastUID(ctx, f.ID, 3); err != nil {
		t.Fatalf("set (lower): %v", err)
	}
	got, err := repo.folderByName(ctx, acc.ID, "INBOX")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.LastUID != 5 {
		t.Fatalf("expected monotonic 5, got %d", got.LastUID)
	}
}

func TestInsertIngest_IsNewThenDuplicate(t *testing.T) {
	repo := setupTestRepo(t)
	cid := communityID(t, repo, "main")
	insertUser(t, repo, "u1")
	f := insertFilter(t, repo, FilterKindDomain, "@acme.com", cid, "u1", false)

	ctx := context.Background()
	acc, _ := repo.EnsureAccount(ctx, AccountConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p", TLSMode: "tls",
	})
	folder, _ := repo.UpsertFolder(ctx, acc.ID, "INBOX", 1000)

	in := IngestInsert{
		FolderID:        folder.ID,
		UID:             42,
		UIDValidity:     1000,
		MessageID:       "<msg-1@acme.com>",
		FromAddr:        "alice@acme.com",
		FromName:        "Alice",
		Subject:         "hello",
		ReceivedAt:      time.Now().UTC(),
		CommunityID:     cid,
		MatchedFilterID: f.ID,
	}
	id1, isNew, err := repo.InsertIngest(ctx, in)
	if err != nil {
		t.Fatalf("insert 1: %v", err)
	}
	if !isNew || id1 == "" {
		t.Fatalf("first insert should be new, got isNew=%v id=%q", isNew, id1)
	}

	id2, isNew, err := repo.InsertIngest(ctx, in)
	if err != nil {
		t.Fatalf("insert 2: %v", err)
	}
	if isNew {
		t.Fatalf("duplicate insert should not be new")
	}
	if id2 != id1 {
		t.Fatalf("duplicate insert should surface original id; got %q vs %q", id2, id1)
	}
}

func TestEnsureAccount_UpdatesOnConfigChange(t *testing.T) {
	repo := setupTestRepo(t)
	ctx := context.Background()
	a1, err := repo.EnsureAccount(ctx, AccountConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p", TLSMode: "tls",
	})
	if err != nil {
		t.Fatalf("ensure 1: %v", err)
	}
	a2, err := repo.EnsureAccount(ctx, AccountConfig{
		Host: "imap.test", Port: 993, Username: "u", Password: "p-new", TLSMode: "tls",
	})
	if err != nil {
		t.Fatalf("ensure 2: %v", err)
	}
	if a1.ID != a2.ID {
		t.Fatalf("singleton invariant broken — different ids %q vs %q", a1.ID, a2.ID)
	}
	if a2.Password != "p-new" {
		t.Fatalf("password update lost: got %q", a2.Password)
	}
}

func TestEnsureAccount_RejectsEmpty(t *testing.T) {
	repo := setupTestRepo(t)
	if _, err := repo.EnsureAccount(context.Background(), AccountConfig{}); err == nil {
		t.Fatalf("empty cfg should error")
	} else if !errors.Is(err, err) { // satisfy 'errors' import even if unwrap unused
		t.Fatalf("unexpected: %v", err)
	}
}
