package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Repo          *auth.Repo
	Svc           *auth.Service
	Chat          *chat.Handler
	Communities   *community.Repo
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

func (h *Handler) cid(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.ID
	}
	return h.CommunityID
}

func (h *Handler) cname(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.Name
	}
	return h.CommunityName
}

func (h *Handler) cslug(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.Slug
	}
	return ""
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.cname(r), CommunitySlug: h.cslug(r)}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the admin dashboard with pending requests, members, invites.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	pending, err := h.Repo.ListPendingMemberships(r.Context(), h.cid(r))
	if err != nil {
		http.Error(w, "load pending: "+err.Error(), http.StatusInternalServerError)
		return
	}
	members, err := h.Repo.ListMembers(r.Context(), h.cid(r))
	if err != nil {
		http.Error(w, "load members: "+err.Error(), http.StatusInternalServerError)
		return
	}
	invites, err := h.Repo.ListInvites(r.Context(), h.cid(r))
	if err != nil {
		http.Error(w, "load invites: "+err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now()
	data := webtempl.AdminPageData{
		Viewer:  h.viewer(r),
		Pending: memberRowsToAdminMembers(pending, now),
		Members: memberRowsToAdminMembers(members, now),
		Invites: invitesToAdminInvites(invites),
	}
	_ = webtempl.AdminPage(data).Render(r.Context(), w)
}

func (h *Handler) PostApprove(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.ApproveMembership(r.Context(), id); err != nil && !errors.Is(err, auth.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.refreshAdminLists(w, r)
}

func (h *Handler) PostReject(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.RejectMembership(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.refreshAdminLists(w, r)
}

type banSignals struct {
	BanHours       int  `json:"ban_hours"`
	CleanupChat    bool `json:"cleanup_chat"`
	CleanupThreads bool `json:"cleanup_threads"`
	CleanupPosts   bool `json:"cleanup_posts"`
}

func (h *Handler) PostBan(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	var in banSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	m, err := h.Repo.MembershipByID(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var until time.Time
	if in.BanHours <= 0 {
		until = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	} else {
		until = time.Now().Add(time.Duration(in.BanHours) * time.Hour)
	}
	if err := h.Repo.UpdateBan(r.Context(), id, &until); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	opts := auth.CleanupOptions{Chat: in.CleanupChat, Threads: in.CleanupThreads, Posts: in.CleanupPosts}
	if opts.Chat || opts.Threads || opts.Posts {
		if err := h.Repo.CleanupUserContent(r.Context(), m.UserID, h.cid(r), opts); err != nil {
			h.Log.Error("cleanup on ban", "err", err)
		}
	}
	// If any chat content was wiped, push a chat fan-out so open chat tabs refresh.
	if opts.Chat && h.Chat != nil {
		h.Chat.Bus.Broadcast()
	}
	h.refreshAdminLists(w, r)
}

func (h *Handler) PostUnban(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.UpdateBan(r.Context(), id, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.refreshAdminLists(w, r)
}

type inviteSignals struct {
	MaxUses string `json:"max_uses"`
}

func (h *Handler) PostInvite(w http.ResponseWriter, r *http.Request) {
	var in inviteSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	var maxUses *int
	if v := strings.TrimSpace(in.MaxUses); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("admin-invite-error", "Max uses must be a positive integer or blank for unlimited"))
			return
		}
		maxUses = &n
	}
	id, _ := auth.FromContext(r.Context())
	creator := id.User.ID
	code, err := h.Svc.IssueInvite(r.Context(), h.cid(r), &creator, maxUses)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("admin-invite-error", err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.AdminInviteCreated(code))
	// Re-render the invite list.
	if list, err := h.Repo.ListInvites(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminInvites(h.cslug(r), invitesToAdminInvites(list)))
	}
}

func (h *Handler) PostInviteRevoke(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	if err := h.Repo.RevokeInvite(r.Context(), code); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := datastar.NewSSE(w, r)
	if list, err := h.Repo.ListInvites(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminInvites(h.cslug(r), invitesToAdminInvites(list)))
	}
}

// refreshAdminLists re-renders #admin-pending and #admin-members after an
// admin action that changed the queue or member list.
func (h *Handler) refreshAdminLists(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	now := time.Now()
	if pending, err := h.Repo.ListPendingMemberships(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminPending(h.cslug(r), memberRowsToAdminMembers(pending, now)))
	}
	if members, err := h.Repo.ListMembers(r.Context(), h.cid(r)); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminMembers(h.cslug(r), memberRowsToAdminMembers(members, now)))
	}
}

type createCommunitySignals struct {
	Name        string `json:"cc_name"`
	Slug        string `json:"cc_slug"`
	MemberEmail string `json:"cc_member_email"`
}

// PostCreateCommunity is the global-admin-only handler for spinning up a new
// community. The slug must be unique; the named user becomes the community's
// first member with role=admin.
func (h *Handler) PostCreateCommunity(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	var in createCommunitySignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "bad signals"))
		return
	}
	name := strings.TrimSpace(in.Name)
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	email := strings.TrimSpace(in.MemberEmail)
	if name == "" || slug == "" || email == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Name, slug and first-member email are required"))
		return
	}
	if !validSlug(slug) {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Slug must contain only a-z, 0-9, '-'"))
		return
	}
	user, err := h.Repo.UserByEmail(r.Context(), email)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "No user with that email"))
		return
	}
	c, err := h.Communities.Create(r.Context(), slug, name)
	if err != nil {
		if errors.Is(err, community.ErrSlugTaken) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Slug already in use"))
			return
		}
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", err.Error()))
		return
	}
	now := time.Now()
	m := auth.Membership{
		ID:          uuid.NewString(),
		UserID:      user.ID,
		CommunityID: c.ID,
		DisplayName: user.Email,
		Role:        auth.RoleAdmin,
		ApprovedAt:  &now,
	}
	if err := h.Repo.CreateMembership(r.Context(), nil, m); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("cc-error", "Could not add first member: "+err.Error()))
		return
	}
	_ = sse.Redirect("/c/" + c.Slug + "/chat")
}

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validSlug(s string) bool { return slugRE.MatchString(s) }

func memberRowsToAdminMembers(rows []auth.MemberRow, now time.Time) []webtempl.AdminMember {
	out := make([]webtempl.AdminMember, 0, len(rows))
	for _, r := range rows {
		am := webtempl.AdminMember{
			MembershipID: r.ID,
			UserID:       r.UserID,
			Email:        r.Email,
			DisplayName:  r.DisplayName,
			Role:         string(r.Role),
			IsBanned:     r.IsBanned(now),
			BannedUntil:  r.BannedUntil,
			IsApproved:   r.IsApproved(),
			CreatedAt:    r.CreatedAt,
		}
		out = append(out, am)
	}
	return out
}

func invitesToAdminInvites(rows []auth.InviteCode) []webtempl.AdminInvite {
	out := make([]webtempl.AdminInvite, 0, len(rows))
	for _, ic := range rows {
		maxStr := "unlimited"
		if ic.MaxUses != nil {
			maxStr = strconv.Itoa(*ic.MaxUses)
		}
		out = append(out, webtempl.AdminInvite{
			Code:       ic.Code,
			MaxUsesStr: maxStr,
			UsesCount:  ic.UsesCount,
			ExpiresAt:  ic.ExpiresAt,
			CreatedAt:  ic.CreatedAt,
		})
	}
	return out
}
