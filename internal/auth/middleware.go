package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"

	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// LoaderLog is the package-level logger used by Loader's diagnostic
// lines. Wired by main.go at boot; nil-safe (no-op when unset).
var LoaderLog *slog.Logger

type ctxKey int

const (
	ctxKeyUser ctxKey = iota + 1
	ctxKeyMembership
	ctxKeyAdminAny
)

type Identity struct {
	User       User
	Membership Membership
}

func FromContext(ctx context.Context) (Identity, bool) {
	u, uok := ctx.Value(ctxKeyUser).(User)
	m, mok := ctx.Value(ctxKeyMembership).(Membership)
	if !uok || !mok {
		return Identity{}, false
	}
	return Identity{User: u, Membership: m}, true
}

func WithIdentity(ctx context.Context, id Identity) context.Context {
	ctx = context.WithValue(ctx, ctxKeyUser, id.User)
	ctx = context.WithValue(ctx, ctxKeyMembership, id.Membership)
	return ctx
}

// WithAdminOfAnyCommunity stashes the per-request flag that powers the
// global /inbox topbar link. Stored under the same key web/templ reads
// from (web/templ is a leaf package, can't import auth — AGENTS §4.13).
func WithAdminOfAnyCommunity(ctx context.Context, v bool) context.Context {
	return context.WithValue(ctx, webtempl.AdminAnyCtxKey(), v)
}

func Loader(sm *scs.SessionManager, repo *Repo) func(http.Handler) http.Handler {
	logDestroy := func(reason, uid, cid, path string, err error) {
		if LoaderLog == nil {
			return
		}
		LoaderLog.Warn("auth.Loader destroying session",
			"reason", reason, "uid", uid, "cid", cid, "path", path, "err", err)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			uid := CurrentUserID(r.Context(), sm)
			cid := CurrentCommunityID(r.Context(), sm)
			if uid == "" || cid == "" {
				next.ServeHTTP(w, r)
				return
			}
			u, err := repo.UserByID(r.Context(), uid)
			if err != nil {
				// Only destroy the session when the user row is GONE
				// (sql.ErrNoRows / our ErrNotFound). For any other
				// error (context.Canceled when the browser walks away,
				// transient DB error, etc.) leave the session alone
				// and just skip identity for this request — the next
				// request that does succeed will set identity again.
				if errors.Is(err, ErrNotFound) {
					logDestroy("user-not-found", uid, cid, r.URL.Path, err)
					_ = Logout(r.Context(), sm)
				}
				next.ServeHTTP(w, r)
				return
			}
			if u.Status != StatusActive {
				logDestroy("user-not-active", uid, cid, r.URL.Path, nil)
				_ = Logout(r.Context(), sm)
				next.ServeHTTP(w, r)
				return
			}
			m, err := repo.MembershipFor(r.Context(), uid, cid)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					logDestroy("membership-not-found", uid, cid, r.URL.Path, err)
					_ = Logout(r.Context(), sm)
				} else if LoaderLog != nil {
					LoaderLog.Warn("auth.Loader membership lookup error",
						"uid", uid, "cid", cid, "path", r.URL.Path, "err", err)
				}
				next.ServeHTTP(w, r)
				return
			}
			if m.IsBanned(time.Now()) {
				logDestroy("user-banned", uid, cid, r.URL.Path, nil)
				_ = Logout(r.Context(), sm)
				http.Redirect(w, r, "/login?banned=1", http.StatusSeeOther)
				return
			}
			ctx := WithIdentity(r.Context(), Identity{User: u, Membership: m})

			// Cheap one-row probe: do we have ANY admin/mod approved
			// membership across communities? Powers the global /inbox
			// link in layout.templ. Errors here log nothing — the
			// caller code paths that need this fall through gracefully.
			if cids, err := repo.AdminCommunityIDs(r.Context(), uid); err == nil && len(cids) > 0 {
				ctx = WithAdminOfAnyCommunity(ctx, true)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func RequireRole(min Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := FromContext(r.Context())
			if !ok {
				http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
				return
			}
			if !id.Membership.Role.AtLeast(min) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireApproved bounces signed-in but unapproved members to /pending.
// Mount this middleware in front of routes that require a fully-active
// member (chat, forum, uploads, profile). Login + /pending itself bypass it.
// Admins always pass — they need to reach /admin to approve queued joins.
func RequireApproved(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok {
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
			return
		}
		if id.Membership.IsApproved() || id.Membership.Role.AtLeast(RoleAdmin) {
			next.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/pending", http.StatusSeeOther)
	})
}
