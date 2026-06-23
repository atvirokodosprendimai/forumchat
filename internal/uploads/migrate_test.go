package uploads_test

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
)

func TestMigrateCommunity(t *testing.T) {
	st, cid, uid := setup(t)
	ctx := context.Background()

	u, err := st.Save(ctx, uid, cid, "text/plain", "note.txt", bytes.NewReader([]byte("hello tenant")))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if u.StoreKey != "" {
		t.Fatalf("new upload on default store should have empty store_key, got %q", u.StoreKey)
	}

	dst := uploads.NewDiskBlobstore(t.TempDir())
	n, err := st.MigrateCommunity(ctx, cid, dst)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 1 {
		t.Fatalf("migrated = %d, want 1", n)
	}

	// Bytes landed in the destination store.
	rc, err := dst.Open(ctx, u.RelPath)
	if err != nil {
		t.Fatalf("open in dst: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "hello tenant" {
		t.Fatalf("dst bytes = %q", got)
	}

	// Row stamped to the community store.
	u2, err := st.Get(ctx, u.ID)
	if err != nil || u2.StoreKey != uploads.StoreKeyCommunity {
		t.Fatalf("store_key after migrate = %q err %v, want %q", u2.StoreKey, err, uploads.StoreKeyCommunity)
	}

	// Idempotent: a second run migrates nothing.
	n2, err := st.MigrateCommunity(ctx, cid, dst)
	if err != nil || n2 != 0 {
		t.Fatalf("re-run migrated = %d err %v, want 0", n2, err)
	}
}
