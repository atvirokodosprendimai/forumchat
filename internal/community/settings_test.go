package community

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/secretbox"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func newTestRepo(t *testing.T) *Repo {
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
	box, _ := secretbox.New("0123456789abcdef0123456789abcdef")
	return &Repo{DB: db, Secrets: box}
}

func TestSettings_RoundTripAndSealing(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	c, err := r.Create(ctx, "acme", "Acme")
	if err != nil {
		t.Fatalf("create community: %v", err)
	}

	// Missing row reads as zero settings (everything unset).
	if s, err := r.Settings(ctx, c.ID); err != nil || s.RAGEnabled != nil || s.RAGQdrantAPIKey != "" {
		t.Fatalf("missing settings must be zero, got %+v err %v", s, err)
	}

	want := Settings{
		CommunityID:     c.ID,
		AIEnabled:       ptrBool(true),
		RAGEnabled:      ptrBool(true),
		RAGEmbedModel:   "e5-large",
		RAGEmbedDim:     4096,
		RAGQdrantURL:    "http://qdrant:6333",
		RAGQdrantAPIKey: "super-secret-key",
		JoinPolicy:      "open",
		StorageBackend:  "s3",
		S3Bucket:        "tenant-private",
		S3AccessKey:     "AKIA-EXAMPLE",
		S3SecretKey:     "shhh",
	}
	if err := r.SaveSettings(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := r.Settings(ctx, c.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.RAGQdrantAPIKey != "super-secret-key" || got.S3SecretKey != "shhh" || got.RAGEmbedDim != 4096 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.AIEnabled == nil || !*got.AIEnabled || got.JoinPolicy != "open" {
		t.Fatalf("bool/string round-trip mismatch: %+v", got)
	}

	// The secret column on disk must NOT contain the plaintext.
	var raw string
	if err := r.DB.QueryRowContext(ctx,
		`SELECT rag_qdrant_api_key_enc FROM community_settings WHERE community_id = ?`, c.ID).
		Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if raw == "super-secret-key" || raw == "" {
		t.Fatalf("secret must be sealed at rest, got %q", raw)
	}
}

func TestSettings_DecryptFailureTolerated(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t) // sealed with key A
	c, _ := r.Create(ctx, "acme", "Acme")
	if err := r.SaveSettings(ctx, Settings{
		CommunityID: c.ID, RAGEmbedModel: "bge-m3", RAGQdrantAPIKey: "secret-A",
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Rotate the key: a new box (different key) can't decrypt the old ciphertext.
	rotated, _ := secretbox.New("FEDCBA9876543210FEDCBA9876543210")
	r.Secrets = rotated

	got, err := r.Settings(ctx, c.ID)
	if err != nil {
		t.Fatalf("decrypt failure must NOT error the load, got %v", err)
	}
	if got.RAGQdrantAPIKey != "" {
		t.Fatalf("undecryptable secret must read empty, got %q", got.RAGQdrantAPIKey)
	}
	if got.RAGEmbedModel != "bge-m3" {
		t.Fatalf("non-secret fields must survive, got model %q", got.RAGEmbedModel)
	}
}
