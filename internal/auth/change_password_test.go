package auth

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

func newPwHandler(t *testing.T) (*Handler, *Repo) {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := NewRepo(db)
	return &Handler{Repo: repo, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, repo
}

// insertUserWithHash inserts an active user carrying the given password_hash and
// returns the id. Pass oauthSentinelHash to model an OAuth-only account.
func insertUserWithHash(t *testing.T, repo *Repo, email, hash string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := repo.DB.ExecContext(context.Background(),
		`INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		 VALUES (?, ?, ?, 'active', 0, 0)`, id, email, hash); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

func postPassword(h *Handler, userID, body string) {
	req := httptest.NewRequest(http.MethodPost, "/profile/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(WithIdentity(req.Context(), Identity{User: User{ID: userID}}))
	h.PostPassword(httptest.NewRecorder(), req)
}

func currentHash(t *testing.T, repo *Repo, userID string) string {
	t.Helper()
	u, err := repo.UserByID(context.Background(), userID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	return u.PasswordHash
}

// TestPostPassword_RegularRequiresCorrectCurrent: a user with a real password
// must supply the correct current one; a wrong current leaves the hash unchanged.
func TestPostPassword_RegularRequiresCorrectCurrent(t *testing.T) {
	h, repo := newPwHandler(t)
	old, _ := HashPassword("oldpassword1")
	uid := insertUserWithHash(t, repo, "u@x.com", old)

	// Wrong current → rejected, hash unchanged.
	postPassword(h, uid, `{"current_password":"WRONG","new_password":"newpassword1","new_password_confirm":"newpassword1"}`)
	if currentHash(t, repo, uid) != old {
		t.Fatalf("wrong current password must NOT change the hash")
	}

	// Correct current → updated, new password verifies, old no longer works.
	postPassword(h, uid, `{"current_password":"oldpassword1","new_password":"newpassword1","new_password_confirm":"newpassword1"}`)
	h2 := currentHash(t, repo, uid)
	if !CheckPassword(h2, "newpassword1") {
		t.Fatalf("new password must verify after change")
	}
	if CheckPassword(h2, "oldpassword1") {
		t.Fatalf("old password must no longer work")
	}
}

// TestPostPassword_OAuthOnlySetsWithoutCurrent: an OAuth-only account (sentinel
// hash) sets a first password without supplying a current one.
func TestPostPassword_OAuthOnlySetsWithoutCurrent(t *testing.T) {
	h, repo := newPwHandler(t)
	uid := insertUserWithHash(t, repo, "social@x.com", oauthSentinelHash)

	postPassword(h, uid, `{"current_password":"","new_password":"brandnew123","new_password_confirm":"brandnew123"}`)

	u, _ := repo.UserByID(context.Background(), uid)
	if !u.HasPassword() {
		t.Fatalf("OAuth-only user must have a usable password after setting one")
	}
	if !CheckPassword(u.PasswordHash, "brandnew123") {
		t.Fatalf("the set password must verify")
	}
}

// TestPostPassword_MismatchAndWeakRejected: confirm-mismatch and too-short
// passwords are rejected and leave the hash unchanged.
func TestPostPassword_MismatchAndWeakRejected(t *testing.T) {
	h, repo := newPwHandler(t)
	old, _ := HashPassword("oldpassword1")
	uid := insertUserWithHash(t, repo, "u@x.com", old)

	postPassword(h, uid, `{"current_password":"oldpassword1","new_password":"newpassword1","new_password_confirm":"different1"}`)
	if currentHash(t, repo, uid) != old {
		t.Fatalf("mismatched confirmation must not change the hash")
	}

	postPassword(h, uid, `{"current_password":"oldpassword1","new_password":"short","new_password_confirm":"short"}`)
	if currentHash(t, repo, uid) != old {
		t.Fatalf("weak (<8) password must not change the hash")
	}
}
