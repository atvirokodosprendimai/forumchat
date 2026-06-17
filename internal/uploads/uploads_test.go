package uploads_test

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
)

func setup(t *testing.T) (*uploads.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ctx := context.Background()
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
	const uid = "00000000-0000-0000-0000-000000000001"
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, ?, 'active', 0, 0)`, uid, "test@example.com", "x"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	uploadDir := filepath.Join(dir, "uploads")
	return uploads.NewStore(db, uploadDir, 1024*1024, "test-key"), c.ID, uid
}

// pngHeader: a 1x1 transparent PNG.
var pngHeader = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
}

func TestSaveAndSign(t *testing.T) {
	t.Parallel()
	store, communityID, ownerID := setup(t)
	ctx := context.Background()
	// Pad with arbitrary bytes so MIME sniff sees PNG header.
	body := append([]byte{}, pngHeader...)
	body = append(body, make([]byte, 200)...)
	u, err := store.Save(ctx, ownerID, communityID, "image/png", "ping.png", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if u.Size != int64(len(body)) {
		t.Fatalf("size mismatch: got %d want %d", u.Size, len(body))
	}
	if u.MIME != "image/png" {
		t.Fatalf("mime: %s", u.MIME)
	}
	if u.Filename != "ping.png" {
		t.Fatalf("filename: %q", u.Filename)
	}
	sig := store.Sign(u.ID, "viewer-1", time.Now().Add(time.Hour))
	if err := store.Verify(u.ID, "viewer-1", sig, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := store.Verify(u.ID, "viewer-2", sig, time.Now().Add(time.Hour).Unix()); err == nil {
		t.Fatal("expected bad sig for different viewer")
	}
}

// TestAcceptArbitraryDoc — denylist policy now lets any non-executable
// MIME through. A PDF claimed under a fuzzy/missing MIME still lands.
func TestAcceptArbitraryDoc(t *testing.T) {
	t.Parallel()
	store, communityID, ownerID := setup(t)
	ctx := context.Background()
	// Minimal PDF header so DetectContentType sniffs "application/pdf".
	body := append([]byte("%PDF-1.4\n"), make([]byte, 64)...)
	u, err := store.Save(ctx, ownerID, communityID, "", "invoice 2026.pdf", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save pdf: %v", err)
	}
	if u.MIME != "application/pdf" {
		t.Fatalf("expected sniffed pdf MIME, got %q", u.MIME)
	}
	if u.Filename != "invoice 2026.pdf" {
		t.Fatalf("filename: %q", u.Filename)
	}
}

// TestRejectExecutable — windows MZ header lands the sniffed MIME on
// the denylist, regardless of what the caller declared.
func TestRejectExecutable(t *testing.T) {
	t.Parallel()
	store, communityID, ownerID := setup(t)
	ctx := context.Background()
	// PE/EXE header — DetectContentType returns application/x-msdownload.
	body := append([]byte{'M', 'Z'}, make([]byte, 256)...)
	if _, err := store.Save(ctx, ownerID, communityID, "image/png", "trojan.exe", bytes.NewReader(body)); err == nil {
		t.Fatal("expected ErrBadMIME on executable sniff")
	}
}

// TestSanitiseFilename — path traversal + control bytes get stripped.
func TestSanitiseFilename(t *testing.T) {
	t.Parallel()
	store, communityID, ownerID := setup(t)
	ctx := context.Background()
	body := append([]byte{}, pngHeader...)
	body = append(body, make([]byte, 32)...)
	u, err := store.Save(ctx, ownerID, communityID, "image/png", "../../etc/pass\x00wd.png", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if u.Filename != "passwd.png" {
		t.Fatalf("expected sanitised filename, got %q", u.Filename)
	}
}

func TestRejectTooLarge(t *testing.T) {
	t.Parallel()
	store, communityID, ownerID := setup(t)
	store.MaxSize = 100
	body := make([]byte, 200)
	copy(body, pngHeader)
	if _, err := store.Save(context.Background(), ownerID, communityID, "image/png", "", bytes.NewReader(body)); err == nil {
		t.Fatal("expected ErrTooLarge")
	}
}
