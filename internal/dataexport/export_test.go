package dataexport_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/dataexport"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"

	"database/sql"
)

func setup(t *testing.T) (*sql.DB, *dataexport.Service) {
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
	store := uploads.NewStore(db, filepath.Join(dir, "uploads"), 1<<20, "k")
	svc := &dataexport.Service{
		Repo:  dataexport.NewRepo(db),
		DB:    db,
		Media: store,
		Dir:   filepath.Join(dir, "exports"),
	}
	return db, svc
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// readZipJSON reads one JSON entry out of the archive and unmarshals it.
func readZipJSON(t *testing.T, path, entry string, v any) {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != entry {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open entry %s: %v", entry, err)
		}
		defer rc.Close()
		b, _ := io.ReadAll(rc)
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("unmarshal %s: %v\n%s", entry, err, b)
		}
		return
	}
	t.Fatalf("entry %s not found in zip", entry)
}

func zipHas(t *testing.T, path, entry string) bool {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name == entry {
			return true
		}
	}
	return false
}

func TestBuild_ScopesRedactsAndArchivesMedia(t *testing.T) {
	t.Parallel()
	db, svc := setup(t)
	ctx := context.Background()
	repo := community.NewRepo(db)

	a, err := repo.Create(ctx, "alpha", "Alpha")
	if err != nil {
		t.Fatalf("community a: %v", err)
	}
	b, err := repo.Create(ctx, "beta", "Beta")
	if err != nil {
		t.Fatalf("community b: %v", err)
	}

	// Two users, one member of each community, with secret password hashes.
	mustExec(t, db, `INSERT INTO users (id, email, password_hash, status, created_at, updated_at) VALUES (?,?,?,?,0,0)`,
		"u-a", "a@x.io", "SECRET-HASH-A", "active")
	mustExec(t, db, `INSERT INTO users (id, email, password_hash, status, created_at, updated_at) VALUES (?,?,?,?,0,0)`,
		"u-b", "b@x.io", "SECRET-HASH-B", "active")
	mustExec(t, db, `INSERT INTO memberships (id, user_id, community_id, display_name, role, created_at) VALUES (?,?,?,?,?,0)`,
		"m-a", "u-a", a.ID, "Alpha User", "owner")
	mustExec(t, db, `INSERT INTO memberships (id, user_id, community_id, display_name, role, created_at) VALUES (?,?,?,?,?,0)`,
		"m-b", "u-b", b.ID, "Beta User", "owner")

	// A webhook (token + signing secret must be redacted) and an AI agent (system
	// prompt must be redacted) in community A.
	mustExec(t, db, `INSERT INTO webhooks (id, community_id, direction, provider, name, token, secret, target_url, created_at) VALUES (?,?,?,?,?,?,?,?,0)`,
		"wh-a", a.ID, "in", "generic", "My hook", "TOKEN-LEAK", "SECRET-LEAK", "")
	mustExec(t, db, `INSERT INTO ai_agents (id, community_id, name, system_prompt, api_key_enc, created_at, updated_at) VALUES (?,?,?,?,?,0,0)`,
		"ag-a", a.ID, "Helper", "YOU-ARE-A-SECRET-PROMPT", "KEY-LEAK")

	// One media file in A, one in B — only A's should be archived.
	store := svc.Media
	if _, err := store.SaveAttachment(ctx, "u-a", a.ID, "text/plain", "alpha-note.txt", bytes.NewReader([]byte("hello alpha"))); err != nil {
		t.Fatalf("save media a: %v", err)
	}
	if _, err := store.SaveAttachment(ctx, "u-b", b.ID, "text/plain", "beta-note.txt", bytes.NewReader([]byte("hello beta"))); err != nil {
		t.Fatalf("save media b: %v", err)
	}

	// Request + build the export for A.
	e, err := svc.Request(ctx, a.ID, "u-a")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	e.Status = dataexport.StatusBuilding
	if err := svc.Build(ctx, e); err != nil {
		t.Fatalf("build: %v", err)
	}

	got, err := svc.Repo.Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != dataexport.StatusReady {
		t.Fatalf("status = %q, want ready (err=%q)", got.Status, got.Error)
	}
	if got.Token == "" || got.ExpiresAt == nil {
		t.Fatalf("ready export missing token/expiry: %+v", got)
	}
	if !got.IsDownloadable(time.Now()) {
		t.Fatal("export should be downloadable")
	}
	zipPath := svc.ZipPath(got)

	// Scoping: members.json has only A's member, never B's.
	var members []map[string]any
	readZipJSON(t, zipPath, "members/memberships.json", &members)
	if len(members) != 1 || members[0]["user_id"] != "u-a" {
		t.Fatalf("memberships scope leak: %+v", members)
	}

	// Redaction: password_hash gone from users.
	var users []map[string]any
	readZipJSON(t, zipPath, "members/members.json", &users)
	if len(users) != 1 {
		t.Fatalf("users scope: %+v", users)
	}
	if _, ok := users[0]["password_hash"]; ok {
		t.Fatal("password_hash leaked into export")
	}
	if users[0]["email"] != "a@x.io" {
		t.Fatalf("wrong user exported: %+v", users[0])
	}

	// Redaction: webhook token + secret gone.
	var hooks []map[string]any
	readZipJSON(t, zipPath, "webhooks/webhooks.json", &hooks)
	if len(hooks) != 1 {
		t.Fatalf("webhooks: %+v", hooks)
	}
	if _, ok := hooks[0]["token"]; ok {
		t.Fatal("webhook token leaked")
	}
	if _, ok := hooks[0]["secret"]; ok {
		t.Fatal("webhook secret leaked")
	}

	// Redaction: agent system prompt + api key gone, name kept.
	var agents []map[string]any
	readZipJSON(t, zipPath, "agents/agents.json", &agents)
	if len(agents) != 1 || agents[0]["name"] != "Helper" {
		t.Fatalf("agents: %+v", agents)
	}
	if _, ok := agents[0]["system_prompt"]; ok {
		t.Fatal("agent system_prompt leaked")
	}
	if _, ok := agents[0]["api_key_enc"]; ok {
		t.Fatal("agent api_key_enc leaked")
	}

	// Media: A's file present, B's absent.
	var meta map[string]any
	readZipJSON(t, zipPath, "manifest.json", &meta)
	if mc, _ := meta["media_files"].(float64); mc != 1 {
		t.Fatalf("media_files = %v, want 1", meta["media_files"])
	}
	ups, _ := store.ListByCommunity(ctx, a.ID)
	if len(ups) != 1 {
		t.Fatalf("expected 1 upload for A, got %d", len(ups))
	}
	if !zipHas(t, zipPath, "media/"+ups[0].ID+"-alpha-note.txt") {
		t.Fatal("alpha media file missing from archive")
	}
}

func TestRequest_RefusesConcurrent(t *testing.T) {
	t.Parallel()
	db, svc := setup(t)
	ctx := context.Background()
	c, err := community.NewRepo(db).Create(ctx, "gamma", "Gamma")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	if _, err := svc.Request(ctx, c.ID, ""); err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := svc.Request(ctx, c.ID, ""); err != dataexport.ErrInProgress {
		t.Fatalf("second request err = %v, want ErrInProgress", err)
	}
}
