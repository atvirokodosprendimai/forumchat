package dataexport_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/dataexport"
)

func TestGetDownload_TokenGating(t *testing.T) {
	t.Parallel()
	db, svc := setup(t)
	ctx := context.Background()
	c, err := community.NewRepo(db).Create(ctx, "delta", "Delta")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	e, err := svc.Request(ctx, c.ID, "")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	e.Status = dataexport.StatusBuilding
	if err := svc.Build(ctx, e); err != nil {
		t.Fatalf("build: %v", err)
	}
	ready, _ := svc.Repo.Get(ctx, e.ID)

	h := &dataexport.Handler{Svc: svc}
	r := chi.NewRouter()
	r.Get("/exports/{id}/download", h.GetDownload)
	srv := httptest.NewServer(r)
	defer srv.Close()

	cases := []struct {
		name string
		url  string
		want int
	}{
		{"valid", "/exports/" + ready.ID + "/download?token=" + ready.Token, http.StatusOK},
		{"wrong token", "/exports/" + ready.ID + "/download?token=deadbeef", http.StatusNotFound},
		{"no token", "/exports/" + ready.ID + "/download", http.StatusNotFound},
		{"unknown id", "/exports/nope/download?token=" + ready.Token, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if tc.want == http.StatusOK {
				if ct := resp.Header.Get("Content-Type"); ct != "application/zip" {
					t.Fatalf("content-type = %q, want application/zip", ct)
				}
				b, _ := io.ReadAll(resp.Body)
				if len(b) < 4 || string(b[:2]) != "PK" {
					t.Fatalf("body is not a zip (len=%d)", len(b))
				}
			}
		})
	}
}

func TestSweep_ExpiresAndDeletes(t *testing.T) {
	t.Parallel()
	db, svc := setup(t)
	ctx := context.Background()
	c, err := community.NewRepo(db).Create(ctx, "epsilon", "Epsilon")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	e, err := svc.Request(ctx, c.ID, "")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	e.Status = dataexport.StatusBuilding
	if err := svc.Build(ctx, e); err != nil {
		t.Fatalf("build: %v", err)
	}
	ready, _ := svc.Repo.Get(ctx, e.ID)
	// Force the expiry into the past, then list-expirable + mark.
	if _, err := db.ExecContext(ctx, `UPDATE community_exports SET expires_at = 1 WHERE id = ?`, ready.ID); err != nil {
		t.Fatalf("expire: %v", err)
	}
	vics, err := svc.Repo.ListExpirable(ctx, ready.RequestedAt)
	if err != nil {
		t.Fatalf("list expirable: %v", err)
	}
	if len(vics) != 1 {
		t.Fatalf("expirable = %d, want 1", len(vics))
	}
	if err := svc.Repo.MarkExpired(ctx, ready.ID); err != nil {
		t.Fatalf("mark expired: %v", err)
	}
	after, _ := svc.Repo.Get(ctx, ready.ID)
	if after.Status != dataexport.StatusExpired {
		t.Fatalf("status = %q, want expired", after.Status)
	}
	if after.IsDownloadable(after.RequestedAt) {
		t.Fatal("expired export must not be downloadable")
	}
}
