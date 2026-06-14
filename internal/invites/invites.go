// Package invites serves the branded /c/{slug}/join?code=... landing page.
// The handler resolves a signup_token into one of four UX branches:
//   - same user already logged in    → one-click confirm
//   - different user logged in       → "sign out to accept"
//   - not logged in, existing account → login form prefilled
//   - not logged in, placeholder user → set-password form
package invites

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	AuthRepo *auth.Repo
	Chat     *chat.Handler
	Sessions *scs.SessionManager
	Log      *slog.Logger
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	if c, ok := community.FromContext(r.Context()); ok {
		v.CommunityName = c.Name
		v.CommunitySlug = c.Slug
	}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetJoin resolves the token and renders one of four branches. The page is
// mounted under LoadCommunity but NOT RequireMember (the whole point is to
// admit a non-member).
func (h *Handler) GetJoin(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	v := h.viewer(r)
	if code == "" {
		_ = webtempl.JoinExpired(v, c.Name).Render(r.Context(), w)
		return
	}
	tok, err := h.AuthRepo.SignupTokenByValue(r.Context(), code)
	if err != nil || !tok.IsValid() || tok.CommunityID != c.ID {
		_ = webtempl.JoinExpired(v, c.Name).Render(r.Context(), w)
		return
	}
	target, err := h.AuthRepo.UserByID(r.Context(), tok.UserID)
	if err != nil {
		_ = webtempl.JoinExpired(v, c.Name).Render(r.Context(), w)
		return
	}

	// Branch on viewer state.
	if id, authed := auth.FromContext(r.Context()); authed {
		if id.User.ID == target.ID {
			_ = webtempl.JoinConfirm(v, c.Name, c.Slug, code).Render(r.Context(), w)
			return
		}
		_ = webtempl.JoinWrongUser(v, c.Name, target.Email).Render(r.Context(), w)
		return
	}
	if target.Status == auth.StatusInvited || target.PasswordHash == "" {
		_ = webtempl.JoinSetPassword(v, c.Name, c.Slug, target.Email, code).Render(r.Context(), w)
		return
	}
	_ = webtempl.JoinLogin(v, c.Name, target.Email, code).Render(r.Context(), w)
}

// PostJoinConfirm — same-user-logged-in click of "Join". Consume the token
// and redirect. Membership row already exists; nothing else to insert.
func (h *Handler) PostJoinConfirm(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, authed := auth.FromContext(r.Context())
	if !authed {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	tok, err := h.AuthRepo.SignupTokenByValue(r.Context(), code)
	if err != nil || !tok.IsValid() || tok.CommunityID != c.ID || tok.UserID != id.User.ID {
		http.Error(w, "invite expired or mismatched", http.StatusForbidden)
		return
	}
	_ = h.AuthRepo.ConsumeSignupToken(r.Context(), code)
	// Welcome ping — existing-user invitee just arrived.
	if h.Chat != nil {
		h.Chat.Welcome(r.Context(), c.ID, id.Membership.DisplayName)
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + c.Slug + "/chat")
}

type setPasswordSignals struct {
	Password string `json:"join_password"`
}

// PostJoinSetPassword activates a placeholder user, consumes the token, logs
// them in, redirects into chat.
func (h *Handler) PostJoinSetPassword(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	var in setPasswordSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("join-error", "bad signals: "+err.Error()))
		return
	}
	sse := render.NewSSE(w, r)
	pw := strings.TrimSpace(in.Password)
	if len(pw) < 8 {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("join-error", "Password must be at least 8 characters"))
		return
	}
	tok, err := h.AuthRepo.SignupTokenByValue(r.Context(), code)
	if err != nil || !tok.IsValid() || tok.CommunityID != c.ID {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("join-error", "Invite expired"))
		return
	}
	target, err := h.AuthRepo.UserByID(r.Context(), tok.UserID)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("join-error", "User not found"))
		return
	}
	if target.Status != auth.StatusInvited {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("join-error", "Account already activated — sign in instead"))
		return
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("join-error", err.Error()))
		return
	}
	if err := h.AuthRepo.SetPasswordAndActivate(r.Context(), target.ID, hash); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("join-error", err.Error()))
		return
	}
	_ = h.AuthRepo.ConsumeSignupToken(r.Context(), code)
	// Log them in.
	auth.PutLogin(r.Context(), h.Sessions, target.ID, c.ID)
	// Welcome ping — new placeholder invitee just activated and joined.
	if h.Chat != nil {
		if m, err := h.AuthRepo.MembershipFor(r.Context(), target.ID, c.ID); err == nil {
			h.Chat.Welcome(r.Context(), c.ID, m.DisplayName)
		}
	}
	_ = sse.Redirect("/c/" + c.Slug + "/chat")
}
