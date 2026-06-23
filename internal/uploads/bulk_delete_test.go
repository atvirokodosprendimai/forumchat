package uploads_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
)

// saveBytes stores a distinct PNG and returns the upload + its on-disk path.
func saveBytes(t *testing.T, store *uploads.Store, ownerID, communityID string, pad int) uploads.Upload {
	t.Helper()
	body := append([]byte{}, pngHeader...)
	body = append(body, make([]byte, pad)...)
	u, err := store.Save(context.Background(), ownerID, communityID, "image/png", "x.png", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(store.PathFor(u)); err != nil {
		t.Fatalf("blob should exist after save: %v", err)
	}
	return u
}

func TestDeleteByCommunity_RemovesRowsAndBlobs(t *testing.T) {
	t.Parallel()
	store, communityID, ownerID := setup(t)
	ctx := context.Background()

	a := saveBytes(t, store, ownerID, communityID, 100)
	b := saveBytes(t, store, ownerID, communityID, 200)

	n, err := store.DeleteByCommunity(ctx, communityID)
	if err != nil {
		t.Fatalf("DeleteByCommunity: %v", err)
	}
	if n != 2 {
		t.Fatalf("removed %d rows, want 2", n)
	}
	for _, u := range []uploads.Upload{a, b} {
		if _, err := store.Get(ctx, u.ID); !errors.Is(err, uploads.ErrNotFound) {
			t.Fatalf("row should be gone, got err: %v", err)
		}
		if _, err := os.Stat(store.PathFor(u)); !os.IsNotExist(err) {
			t.Fatalf("blob should be gone, stat err: %v", err)
		}
	}
}

func TestDeleteByOwner_RemovesRowsAndBlobs(t *testing.T) {
	t.Parallel()
	store, communityID, ownerID := setup(t)
	ctx := context.Background()

	u := saveBytes(t, store, ownerID, communityID, 300)

	n, err := store.DeleteByOwner(ctx, ownerID)
	if err != nil {
		t.Fatalf("DeleteByOwner: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed %d rows, want 1", n)
	}
	if _, err := store.Get(ctx, u.ID); !errors.Is(err, uploads.ErrNotFound) {
		t.Fatalf("row should be gone, got err: %v", err)
	}
	if _, err := os.Stat(store.PathFor(u)); !os.IsNotExist(err) {
		t.Fatalf("blob should be gone, stat err: %v", err)
	}
}
