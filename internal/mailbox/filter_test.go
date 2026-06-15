package mailbox

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func setupTestRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Bootstrap a community so FK constraints on filters/ingest pass.
	if _, err := community.NewRepo(db).BootstrapOrFetch(ctx, "main", "Main"); err != nil {
		t.Fatalf("community: %v", err)
	}
	return NewRepo(db)
}

func insertFilter(t *testing.T, r *Repo, kind FilterKind, pattern, communityID, createdBy string, toIssue bool) Filter {
	t.Helper()
	ctx := context.Background()
	id := "f-" + pattern // deterministic for test diffs
	tof := 0
	if toIssue {
		tof = 1
	}
	if _, err := r.DB.ExecContext(ctx, `
		INSERT INTO community_mail_filter (id, community_id, kind, pattern, to_issue, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0)`,
		id, communityID, string(kind), pattern, tof, createdBy); err != nil {
		t.Fatalf("insert filter: %v", err)
	}
	r.InvalidateFilters()
	return Filter{ID: id, CommunityID: communityID, Kind: kind, Pattern: pattern, ToIssue: toIssue}
}

func insertUser(t *testing.T, r *Repo, id string) {
	t.Helper()
	if _, err := r.DB.ExecContext(context.Background(), `
		INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, '', 'verified', 0, 0)`, id, id+"@local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func communityID(t *testing.T, r *Repo, slug string) string {
	t.Helper()
	c, err := community.NewRepo(r.DB).BySlug(context.Background(), slug)
	if err != nil {
		t.Fatalf("community lookup: %v", err)
	}
	return c.ID
}

func TestMatchFrom_ExactBeatsDomain(t *testing.T) {
	repo := setupTestRepo(t)
	cid := communityID(t, repo, "main")
	insertUser(t, repo, "u1")

	// Build another community so we can verify routing actually picks the
	// right row.
	other, err := community.NewRepo(repo.DB).Create(context.Background(), "other", "Other")
	if err != nil {
		t.Fatalf("create community other: %v", err)
	}

	insertFilter(t, repo, FilterKindDomain, "@acme.com", cid, "u1", false)
	insertFilter(t, repo, FilterKindAddress, "alice@acme.com", other.ID, "u1", true)

	f, ok, err := MatchFrom(context.Background(), repo, "Alice@Acme.COM")
	if err != nil || !ok {
		t.Fatalf("expected match, got ok=%v err=%v", ok, err)
	}
	if f.CommunityID != other.ID || f.Kind != FilterKindAddress {
		t.Fatalf("exact filter should win; got %+v", f)
	}

	f, ok, err = MatchFrom(context.Background(), repo, "bob@acme.com")
	if err != nil || !ok {
		t.Fatalf("domain match: ok=%v err=%v", ok, err)
	}
	if f.Kind != FilterKindDomain || f.CommunityID != cid {
		t.Fatalf("domain filter expected; got %+v", f)
	}
}

func TestMatchFrom_NoMatchAndMalformed(t *testing.T) {
	repo := setupTestRepo(t)
	cid := communityID(t, repo, "main")
	insertUser(t, repo, "u1")
	insertFilter(t, repo, FilterKindDomain, "@acme.com", cid, "u1", false)

	for _, addr := range []string{"", "no-at-sign", "stranger@other.tld", "  "} {
		_, ok, err := MatchFrom(context.Background(), repo, addr)
		if err != nil {
			t.Fatalf("err for %q: %v", addr, err)
		}
		if ok {
			t.Fatalf("expected no match for %q", addr)
		}
	}
}

func TestNormaliseFilterPattern(t *testing.T) {
	cases := []struct {
		kind FilterKind
		in   string
		want string
	}{
		{FilterKindAddress, "Alice@Acme.com", "alice@acme.com"},
		{FilterKindAddress, "no-at", ""},
		{FilterKindAddress, "@nofreq.com", ""},
		{FilterKindDomain, "Acme.com", "@acme.com"},
		{FilterKindDomain, "*@Acme.com", "@acme.com"},
		{FilterKindDomain, "@Acme.com", "@acme.com"},
		{FilterKindDomain, "", ""},
		{FilterKindDomain, "@", ""},
		{FilterKindDomain, "foo@bar.com", ""}, // already has user part — reject
	}
	for _, c := range cases {
		got := normaliseFilterPattern(c.kind, c.in)
		if got != c.want {
			t.Fatalf("normalise(%s, %q) = %q, want %q", c.kind, c.in, got, c.want)
		}
	}
}
