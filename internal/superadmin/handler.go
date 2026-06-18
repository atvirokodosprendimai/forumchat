// Package superadmin implements the global platform super-admin surface:
// a god-mode dashboard over every community and user, gated by
// auth.RequireSuperAdmin (the SUPERADMIN_EMAILS allowlist). Per-community
// administration still happens in each community's own /admin — a
// super-admin reaches those via the RequireMember bypass.
package superadmin

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	AuthRepo    *auth.Repo
	Communities *community.Repo
	Log         *slog.Logger
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validSlug(s string) bool { return slugRE.MatchString(s) }

func localPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the platform dashboard: every community and every user.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	comms, err := h.Communities.ListAll(r.Context())
	if err != nil {
		http.Error(w, "load communities: "+err.Error(), http.StatusInternalServerError)
		return
	}
	users, err := h.AuthRepo.ListAllUsers(r.Context())
	if err != nil {
		http.Error(w, "load users: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data := webtempl.SuperAdminPageData{
		Viewer:      h.viewer(r),
		Communities: toSACommunities(comms),
		Users:       toSAUsers(users),
	}
	_ = webtempl.SuperAdminPage(data).Render(r.Context(), w)
}

const dateFmt = "2006-01-02"

func toSACommunities(in []community.CommunityStat) []webtempl.SACommunity {
	out := make([]webtempl.SACommunity, 0, len(in))
	for _, c := range in {
		out = append(out, webtempl.SACommunity{
			ID:          c.ID,
			Slug:        c.Slug,
			Name:        c.Name,
			IsPublic:    c.IsPublic,
			MemberCount: c.MemberCount,
			CreatedAt:   c.CreatedAt.Format(dateFmt),
		})
	}
	return out
}

func toSAUsers(in []auth.GlobalUser) []webtempl.SAUser {
	out := make([]webtempl.SAUser, 0, len(in))
	for _, u := range in {
		out = append(out, webtempl.SAUser{
			ID:          u.ID,
			Email:       u.Email,
			Status:      string(u.Status),
			Communities: u.CommunityCount,
			CreatedAt:   u.CreatedAt.Format(dateFmt),
		})
	}
	return out
}

type createSignals struct {
	Name  string `json:"sa_name"`
	Slug  string `json:"sa_slug"`
	Email string `json:"sa_email"`
}

// PostCreateCommunity spins up a new community and makes the named existing
// user its first admin. Mirrors admin.PostCreateCommunity but lives behind
// the platform-wide super-admin gate.
func (h *Handler) PostCreateCommunity(w http.ResponseWriter, r *http.Request) {
	var in createSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	name := strings.TrimSpace(in.Name)
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	email := strings.TrimSpace(in.Email)
	if name == "" || slug == "" || email == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "Name, slug and first-admin email are required"))
		return
	}
	if !validSlug(slug) {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "Slug must contain only a-z, 0-9, '-'"))
		return
	}
	user, err := h.AuthRepo.UserByEmail(r.Context(), email)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "No user with that email"))
		return
	}
	c, err := h.Communities.Create(r.Context(), slug, name)
	if err != nil {
		if errors.Is(err, community.ErrSlugTaken) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "Slug already in use"))
			return
		}
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", err.Error()))
		return
	}
	now := time.Now()
	m := auth.Membership{
		ID:          uuid.NewString(),
		UserID:      user.ID,
		CommunityID: c.ID,
		DisplayName: localPart(user.Email),
		Role:        auth.RoleAdmin,
		ApprovedAt:  &now,
	}
	if err := h.AuthRepo.CreateMembership(r.Context(), nil, m); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-cc-error", "Could not add first admin: "+err.Error()))
		return
	}
	_ = sse.Redirect("/superadmin")
}

type cidSignals struct {
	CID string `json:"sa_cid"`
}

// PostDeleteCommunity removes a community. Foreign-key constraints make this
// succeed only for an empty community; otherwise the DB error is surfaced.
func (h *Handler) PostDeleteCommunity(w http.ResponseWriter, r *http.Request) {
	var in cidSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	if strings.TrimSpace(in.CID) == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No community selected"))
		return
	}
	if err := h.Communities.Delete(r.Context(), in.CID); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Delete refused — community still has members or content: "+err.Error()))
		return
	}
	_ = sse.Redirect("/superadmin")
}

type uidSignals struct {
	UID string `json:"sa_uid"`
}

// PostDisableUser disables an account platform-wide. auth.Loader signs the
// user out on their next request. A super-admin can't disable themselves.
func (h *Handler) PostDisableUser(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, auth.StatusDisabled)
}

// PostEnableUser re-activates a disabled account.
func (h *Handler) PostEnableUser(w http.ResponseWriter, r *http.Request) {
	h.setStatus(w, r, auth.StatusActive)
}

func (h *Handler) setStatus(w http.ResponseWriter, r *http.Request, status auth.UserStatus) {
	var in uidSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	uid := strings.TrimSpace(in.UID)
	if uid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No user selected"))
		return
	}
	if status == auth.StatusDisabled {
		if id, ok := auth.FromContext(r.Context()); ok && id.User.ID == uid {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "You can't disable your own account"))
			return
		}
	}
	if err := h.AuthRepo.SetUserStatus(r.Context(), uid, status); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Update failed: "+err.Error()))
		return
	}
	_ = sse.Redirect("/superadmin")
}
