package uploads

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// TestDiskBlobstore_Contract exercises the Blobstore contract the S3 impl must
// also satisfy: Put is idempotent, Open round-trips, Exists/Remove behave, and
// LocalPath is available for disk.
func TestDiskBlobstore_Contract(t *testing.T) {
	ctx := context.Background()
	bs := NewDiskBlobstore(t.TempDir())
	const key = "ab/abcdef.bin"
	body := []byte("hello blobstore")

	if ok, _ := bs.Exists(ctx, key); ok {
		t.Fatal("key must not exist before Put")
	}
	if err := bs.Put(ctx, key, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Idempotent: second Put with same key is a no-op, no error.
	if err := bs.Put(ctx, key, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put idempotent: %v", err)
	}
	if ok, err := bs.Exists(ctx, key); err != nil || !ok {
		t.Fatalf("Exists after Put = %v, %v", ok, err)
	}
	rc, err := bs.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip mismatch: %q", got)
	}
	if p, ok := bs.LocalPath(key); !ok || p == "" {
		t.Fatal("disk LocalPath must be available")
	}
	if err := bs.Remove(ctx, key); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if ok, _ := bs.Exists(ctx, key); ok {
		t.Fatal("key must be gone after Remove")
	}
	// Remove of a missing key is not an error.
	if err := bs.Remove(ctx, key); err != nil {
		t.Fatalf("Remove missing: %v", err)
	}
}
