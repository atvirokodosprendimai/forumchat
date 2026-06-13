package community

import (
	"errors"
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

// LoadCommunity reads {slug} from the chi URL params, looks up the community
// via repo, and attaches it to the request context for downstream handlers
// (use FromContext / MustFromContext). 404s if the slug is unknown.
func LoadCommunity(repo *Repo) func(http.Handler) http.Handler {
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
			next.ServeHTTP(w, r.WithContext(WithContext(r.Context(), c)))
		})
	}
}

// RequireMember rejects requests whose viewer has no membership in the
// resolved community. Global admins are not auto-admitted — they need a
// membership row to access community content.
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
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			// Repopulate identity with the per-community membership so
			// downstream handlers see the right role / display name.
			r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{User: id.User, Membership: m}))
			next.ServeHTTP(w, r)
		})
	}
}
