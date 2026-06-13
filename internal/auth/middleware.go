package auth

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
)

type ctxKey int

const (
	ctxKeyUser ctxKey = iota + 1
	ctxKeyMembership
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

func Loader(sm *scs.SessionManager, repo *Repo) func(http.Handler) http.Handler {
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
				_ = Logout(r.Context(), sm)
				next.ServeHTTP(w, r)
				return
			}
			if u.Status != StatusActive {
				_ = Logout(r.Context(), sm)
				next.ServeHTTP(w, r)
				return
			}
			m, err := repo.MembershipFor(r.Context(), uid, cid)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					_ = Logout(r.Context(), sm)
				}
				next.ServeHTTP(w, r)
				return
			}
			if m.IsBanned(time.Now()) {
				_ = Logout(r.Context(), sm)
				http.Redirect(w, r, "/login?banned=1", http.StatusSeeOther)
				return
			}
			ctx := WithIdentity(r.Context(), Identity{User: u, Membership: m})
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
