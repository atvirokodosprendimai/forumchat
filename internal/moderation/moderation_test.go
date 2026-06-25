package moderation

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func TestParseGuardVerdict(t *testing.T) {
	cases := []struct {
		name     string
		reply    string
		flagged  bool
		wantCats []string
	}{
		{"safe", "safe", false, nil},
		{"safe with trailing newline", "safe\n", false, nil},
		{"unsafe single category", "unsafe\nS12", true, []string{"S12"}},
		{"unsafe multi category", "unsafe\nS3,S12", true, []string{"S3", "S12"}},
		{"unsafe spaced", "unsafe\n S3, S12 ", true, []string{"S3", "S12"}},
		{"unsafe dedupe", "unsafe\nS12,S12", true, []string{"S12"}},
		{"garbled stays safe", "I cannot help with that", false, nil},
		{"uppercase normalized", "UNSAFE\ns4", true, []string{"S4"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := parseGuardVerdict(c.reply)
			if v.Flagged != c.flagged {
				t.Fatalf("flagged = %v, want %v (reply=%q)", v.Flagged, c.flagged, c.reply)
			}
			if !c.flagged {
				return
			}
			if !reflect.DeepEqual(v.Categories, c.wantCats) {
				t.Fatalf("categories = %v, want %v", v.Categories, c.wantCats)
			}
		})
	}
}

func TestCategoryLabel(t *testing.T) {
	if got := CategoryLabel("S4"); got != "Child sexual exploitation" {
		t.Errorf("S4 label = %q", got)
	}
	if got := CategoryLabel("s12"); got != "Sexual content" {
		t.Errorf("s12 label = %q (case-insensitive expected)", got)
	}
	if got := CategoryLabel("S99"); got != "S99" {
		t.Errorf("unknown code must echo back, got %q", got)
	}
}

func TestRepoInsert(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// community_id FK requires a real community row.
	if _, err := db.ExecContext(ctx, `INSERT INTO communities (id, slug, name, created_at) VALUES ('c1','c','C',0)`); err != nil {
		t.Fatalf("seed community: %v", err)
	}
	r := NewRepo(db)
	if err := r.Insert(ctx, Flag{CommunityID: "c1", MessageID: "m1", Categories: "S3,S12", Model: "llama-guard3:1b"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	var cats string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(categories),'') FROM moderation_flags WHERE community_id='c1'`).Scan(&n, &cats); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n != 1 || cats != "S3,S12" {
		t.Fatalf("got n=%d cats=%q, want 1 / S3,S12", n, cats)
	}
}
