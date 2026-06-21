package debuglog_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/debuglog"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func newRecorder(t *testing.T) *debuglog.Recorder {
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
	return debuglog.New(db, nil)
}

func TestRecordGatedByEnabled(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)

	if r.Enabled() {
		t.Fatal("recorder should start disabled")
	}

	// Disabled → no-op write.
	r.Record(ctx, "webhook", "inbound", "github push", []byte(`{"a":1}`), nil)
	if n, err := r.Count(ctx); err != nil || n != 0 {
		t.Fatalf("disabled write should not persist: n=%d err=%v", n, err)
	}

	// Enable → write lands.
	r.SetEnabled(true)
	if !r.Enabled() {
		t.Fatal("Enabled() should be true after SetEnabled(true)")
	}
	r.Record(ctx, "webhook", "inbound", "github push", []byte(`{"a":1}`), map[string]string{"provider": "github"})
	n, err := r.Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("enabled write should persist: n=%d err=%v", n, err)
	}

	entries, err := r.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Source != "webhook" || e.Event != "inbound" || e.Summary != "github push" {
		t.Fatalf("unexpected entry fields: %+v", e)
	}
	if e.Payload != `{"a":1}` {
		t.Fatalf("payload mismatch: %q", e.Payload)
	}
	if e.Meta != `{"provider":"github"}` {
		t.Fatalf("meta mismatch: %q", e.Meta)
	}

	// Disable again → further writes are dropped.
	r.SetEnabled(false)
	r.Record(ctx, "webhook", "outbound", "x", []byte("y"), nil)
	if n, _ := r.Count(ctx); n != 1 {
		t.Fatalf("write after disable should be dropped: n=%d", n)
	}
}

func TestClear(t *testing.T) {
	ctx := context.Background()
	r := newRecorder(t)
	r.SetEnabled(true)
	for range 3 {
		r.Record(ctx, "webhook", "inbound", "s", []byte("p"), nil)
	}
	if n, _ := r.Count(ctx); n != 3 {
		t.Fatalf("want 3 before clear, got %d", n)
	}
	if err := r.Clear(ctx); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n, _ := r.Count(ctx); n != 0 {
		t.Fatalf("want 0 after clear, got %d", n)
	}
}

func TestNilReceiverSafe(t *testing.T) {
	var r *debuglog.Recorder
	ctx := context.Background()
	// None of these should panic on a nil recorder.
	if r.Enabled() {
		t.Fatal("nil recorder should report disabled")
	}
	r.SetEnabled(true)
	r.Record(ctx, "webhook", "inbound", "s", []byte("p"), nil)
	if n, err := r.Count(ctx); err != nil || n != 0 {
		t.Fatalf("nil Count: n=%d err=%v", n, err)
	}
	if entries, err := r.List(ctx); err != nil || entries != nil {
		t.Fatalf("nil List: %v %v", entries, err)
	}
	if err := r.Clear(ctx); err != nil {
		t.Fatalf("nil Clear: %v", err)
	}
}
