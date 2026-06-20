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
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	AuthRepo    *auth.Repo
	Communities *community.Repo
	Log         *slog.Logger
	// Bus fans out a chat refresh after a system-ban wipes content so open
	// chat tabs drop the soft-deleted messages live. Nil-safe (tests omit it).
	Bus *chat.Bus
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
			ID:           c.ID,
			Slug:         c.Slug,
			Name:         c.Name,
			IsPublic:     c.IsPublic,
			MemberCount:  c.MemberCount,
			MessageCount: c.MessageCount,
			ThreadCount:  c.ThreadCount,
			CreatedAt:    c.CreatedAt.Format(dateFmt),
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

type deleteSignals struct {
	CID         string `json:"sa_cid"`
	ConfirmSlug string `json:"sa_confirm_slug"`
}

// PostDeleteCommunity permanently deletes a community AND cascades its
// content (see community.Repo.Delete — this is destructive, not a safe
// no-op). To prevent accidental nukes it requires the caller to type the
// community's slug back: the delete proceeds only when sa_confirm_slug
// exactly matches the target community's slug. The action is audit-logged.
func (h *Handler) PostDeleteCommunity(w http.ResponseWriter, r *http.Request) {
	var in deleteSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	cid := strings.TrimSpace(in.CID)
	if cid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No community selected"))
		return
	}
	c, err := h.Communities.ByID(r.Context(), cid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No such community"))
		return
	}
	// Server-side guard — never trust the client prompt. The typed slug must
	// match exactly, or we refuse without touching the database.
	if strings.TrimSpace(in.ConfirmSlug) != c.Slug {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result",
			"Delete cancelled — the typed slug did not match \""+c.Slug+"\". Nothing was deleted."))
		return
	}
	if err := h.Communities.Delete(r.Context(), cid); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Delete failed: "+err.Error()))
		return
	}
	actor := "unknown"
	if id, ok := auth.FromContext(r.Context()); ok {
		actor = id.User.Email
	}
	if h.Log != nil {
		h.Log.Warn("super-admin deleted community (cascaded)",
			"actor", actor, "community_id", cid, "slug", c.Slug, "name", c.Name)
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

func toSAUserMemberships(in []auth.UserMembership) []webtempl.SAUserMembership {
	now := time.Now()
	out := make([]webtempl.SAUserMembership, 0, len(in))
	for _, m := range in {
		row := webtempl.SAUserMembership{
			MembershipID: m.MembershipID,
			Slug:         m.Slug,
			Name:         m.Name,
			Role:         string(m.Role),
			IsApproved:   m.IsApproved,
			IsBanned:     m.BannedUntil != nil && m.BannedUntil.After(now),
			ChatCount:    m.ChatCount,
			ThreadCount:  m.ThreadCount,
		}
		if m.LastActive != nil {
			row.LastActive = m.LastActive.Format(dateFmt)
		}
		out = append(out, row)
	}
	return out
}

// renderMemberships re-renders the drill-down fragment for one user. Used by
// the expand GET and after every per-community action so the panel reflects
// the new state in place (live morph — keeps the row expanded, §4.7).
func (h *Handler) renderMemberships(sse *datastar.ServerSentEventGenerator, r *http.Request, uid string) {
	mems, err := h.AuthRepo.UserMemberships(r.Context(), uid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "load memberships: "+err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.SAUserMemberships(uid, toSAUserMemberships(mems)))
}

// GetUserMemberships lazily loads the per-user community drill-down: which
// communities the user belongs to, their role + state in each, basic activity
// counts, and per-community ban/remove actions. Answers "this user shows 2
// communities — which 2?".
func (h *Handler) GetUserMemberships(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	sse := render.NewSSE(w, r)
	if uid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No user selected"))
		return
	}
	h.renderMemberships(sse, r, uid)
}

type memActionSignals struct {
	MID      string `json:"sa_mid"`
	UID      string `json:"sa_uid"`
	BanHours int    `json:"ban_hours"`
}

// PostCommunityBan bans a user from one community (permanent unless ban_hours
// is set), targeting the membership directly. Re-renders the drill-down so the
// row flips to "banned" live.
func (h *Handler) PostCommunityBan(w http.ResponseWriter, r *http.Request) {
	var in memActionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	mid := strings.TrimSpace(in.MID)
	if mid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No membership selected"))
		return
	}
	m, err := h.AuthRepo.MembershipByID(r.Context(), mid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No such membership"))
		return
	}
	var until time.Time
	if in.BanHours <= 0 {
		until = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		until = time.Now().Add(time.Duration(in.BanHours) * time.Hour)
	}
	if err := h.AuthRepo.UpdateBan(r.Context(), mid, &until); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Ban failed: "+err.Error()))
		return
	}
	h.audit(r, "super-admin banned user from community", "membership_id", mid, "community_id", m.CommunityID, "user_id", m.UserID)
	h.renderMemberships(sse, r, m.UserID)
}

// PostCommunityRemove hard-deletes one membership row (the account is kept).
// Guarded so a community can't be orphaned by removing its last admin.
func (h *Handler) PostCommunityRemove(w http.ResponseWriter, r *http.Request) {
	var in memActionSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	mid := strings.TrimSpace(in.MID)
	if mid == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No membership selected"))
		return
	}
	m, err := h.AuthRepo.MembershipByID(r.Context(), mid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "No such membership"))
		return
	}
	if m.Role == auth.RoleAdmin {
		count, err := h.AuthRepo.CountAdmins(r.Context(), m.CommunityID)
		if err != nil {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "count admins: "+err.Error()))
			return
		}
		if count <= 1 {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Refused — this is the community's last admin. Promote another admin first."))
			return
		}
	}
	if err := h.AuthRepo.RejectMembership(r.Context(), mid); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Remove failed: "+err.Error()))
		return
	}
	h.audit(r, "super-admin removed user from community", "membership_id", mid, "community_id", m.CommunityID, "user_id", m.UserID)
	h.renderMemberships(sse, r, m.UserID)
}

// PostSystemBan is the platform-wide kill switch: disables the account (the
// user is signed out on their next request) AND soft-deletes all their chat,
// threads and posts across every community they belong to. A super-admin
// can't system-ban themselves.
func (h *Handler) PostSystemBan(w http.ResponseWriter, r *http.Request) {
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
	if id, ok := auth.FromContext(r.Context()); ok && id.User.ID == uid {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "You can't system-ban your own account"))
		return
	}
	if err := h.AuthRepo.SetUserStatus(r.Context(), uid, auth.StatusDisabled); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Disable failed: "+err.Error()))
		return
	}
	mems, err := h.AuthRepo.UserMemberships(r.Context(), uid)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("sa-result", "Account disabled, but loading communities to wipe failed: "+err.Error()))
		return
	}
	opts := auth.CleanupOptions{Chat: true, Threads: true, Posts: true}
	for _, m := range mems {
		if err := h.AuthRepo.CleanupUserContent(r.Context(), uid, m.CommunityID, opts); err != nil && h.Log != nil {
			h.Log.Error("system-ban cleanup", "err", err, "user_id", uid, "community_id", m.CommunityID)
		}
	}
	if h.Bus != nil {
		h.Bus.Broadcast("")
	}
	h.audit(r, "super-admin system-banned user (disabled + content wiped)", "user_id", uid, "communities", len(mems))
	_ = sse.Redirect("/superadmin")
}

// audit logs a destructive super-admin action with the acting account's email.
func (h *Handler) audit(r *http.Request, msg string, kv ...any) {
	if h.Log == nil {
		return
	}
	actor := "unknown"
	if id, ok := auth.FromContext(r.Context()); ok {
		actor = id.User.Email
	}
	h.Log.Warn(msg, append([]any{"actor", actor}, kv...)...)
}
