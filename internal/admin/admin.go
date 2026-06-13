package admin

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Repo          *auth.Repo
	Svc           *auth.Service
	Chat          *chat.Handler
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.CommunityName}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the admin dashboard with pending requests, members, invites.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	pending, err := h.Repo.ListPendingMemberships(r.Context(), h.CommunityID)
	if err != nil {
		http.Error(w, "load pending: "+err.Error(), http.StatusInternalServerError)
		return
	}
	members, err := h.Repo.ListMembers(r.Context(), h.CommunityID)
	if err != nil {
		http.Error(w, "load members: "+err.Error(), http.StatusInternalServerError)
		return
	}
	invites, err := h.Repo.ListInvites(r.Context(), h.CommunityID)
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
		if err := h.Repo.CleanupUserContent(r.Context(), m.UserID, h.CommunityID, opts); err != nil {
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
	code, err := h.Svc.IssueInvite(r.Context(), h.CommunityID, &creator, maxUses)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("admin-invite-error", err.Error()))
		return
	}
	_ = sse.PatchElementTempl(webtempl.AdminInviteCreated(code))
	// Re-render the invite list.
	if list, err := h.Repo.ListInvites(r.Context(), h.CommunityID); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminInvites(invitesToAdminInvites(list)))
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
	if list, err := h.Repo.ListInvites(r.Context(), h.CommunityID); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminInvites(invitesToAdminInvites(list)))
	}
}

// refreshAdminLists re-renders #admin-pending and #admin-members after an
// admin action that changed the queue or member list.
func (h *Handler) refreshAdminLists(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	now := time.Now()
	if pending, err := h.Repo.ListPendingMemberships(r.Context(), h.CommunityID); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminPending(memberRowsToAdminMembers(pending, now)))
	}
	if members, err := h.Repo.ListMembers(r.Context(), h.CommunityID); err == nil {
		_ = sse.PatchElementTempl(webtempl.AdminMembers(memberRowsToAdminMembers(members, now)))
	}
}

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
