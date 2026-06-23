package community

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// LoadCommunity reads {slug} from the chi URL params, looks up the community
// via repo, and attaches it to the request context for downstream handlers
// (use FromContext / MustFromContext). 404s if the slug is unknown. It also
// stamps the per-community "AI enabled" flag (read by webtempl.CommunityAIEnabled
// for the Agent nav). In self-hosted mode this is just the global AI_ENABLED —
// no extra DB read; only SaaS loads community_settings to resolve the override.
func LoadCommunity(repo *Repo, cfg config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slug := chi.URLParam(r, "slug")
			if slug == "" {
				http.NotFound(w, r)
				return
			}
			c, err := repo.BySlug(r.Context(), slug)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					http.NotFound(w, r)
					return
				}
				http.Error(w, "load community: "+err.Error(), http.StatusInternalServerError)
				return
			}
			aiEnabled := cfg.AIEnabled
			if cfg.SAAS && aiEnabled {
				if s, err := repo.Settings(r.Context(), c.ID); err == nil {
					aiEnabled = EffectiveAIEnabled(s, cfg)
				}
			}
			ctx := WithContext(r.Context(), c)
			ctx = context.WithValue(ctx, webtempl.CommunityAICtxKey(), aiEnabled)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireMember rejects requests whose viewer has no membership in the
// resolved community. Ordinary global/community admins are not
// auto-admitted — they need a membership row. The one exception is a
// platform super-admin (SUPERADMIN_EMAILS): when they have no row we
// synthesize an approved admin membership so god-mode reaches every
// community's admin surface without a join.
func RequireMember(authRepo *auth.Repo) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := FromContext(r.Context())
			if !ok {
				http.NotFound(w, r)
				return
			}
			id, ok := auth.FromContext(r.Context())
			if !ok {
				http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
				return
			}
			m, err := authRepo.MembershipFor(r.Context(), id.User.ID, c.ID)
			if err != nil {
				if id.IsSuperAdmin && errors.Is(err, auth.ErrNotFound) {
					m = auth.SuperAdminMembership(id.User, c.ID)
				} else {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
			// auth.Loader checks the ban only for the SESSION community; this
			// rebinds to the URL-slug community, so a user banned in community A
			// whose session is pinned to B would otherwise still reach A. Enforce
			// the per-community ban here. (Super-admins synthesize an unbanned
			// membership above, so god-mode is unaffected.)
			if m.IsBanned(time.Now()) {
				http.Redirect(w, r, "/login?banned=1", http.StatusSeeOther)
				return
			}
			// Repopulate identity with the per-community membership so
			// downstream handlers see the right role / display name.
			// Preserve the super-admin flag across the rebind.
			r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{User: id.User, Membership: m, IsSuperAdmin: id.IsSuperAdmin}))
			next.ServeHTTP(w, r)
		})
	}
}
