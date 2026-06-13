package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"
	datastar "github.com/starfederation/datastar-go/datastar"

	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Svc           *Service
	Repo          *Repo
	Sessions      *scs.SessionManager
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

type chiMux interface {
	Get(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
}

func (h *Handler) Mount(r chiMux) {
	r.Get("/register", h.GetRegister)
	r.Post("/register", h.PostRegister)
	r.Get("/verify", h.GetVerify)
	r.Get("/login", h.GetLogin)
	r.Post("/login", h.PostLogin)
	r.Post("/logout", h.PostLogout)
}

// Viewer derives the current Viewer used by templ.Layout from a request.
func (h *Handler) Viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.CommunityName}
	if id, ok := FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// --- register ---

func (h *Handler) GetRegister(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.RegisterPage().Render(r.Context(), w)
}

type registerSignals struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	InviteCode string `json:"invite_code"`
}

func (h *Handler) PostRegister(w http.ResponseWriter, r *http.Request) {
	var in registerSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	email := strings.TrimSpace(in.Email)
	invite := strings.TrimSpace(strings.ToUpper(in.InviteCode))
	if email == "" || in.Password == "" || invite == "" {
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment("All fields required"))
		return
	}
	res, err := h.Svc.Register(r.Context(), RegisterInput{Email: email, Password: in.Password, InviteCode: invite})
	if err != nil {
		msg := registerErrMsg(err)
		if msg == "" {
			h.Log.Error("register failed", "err", err)
			msg = "Something went wrong"
		}
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment(msg))
		return
	}
	h.Log.Info("user registered", "user_id", res.UserID, "verify_url", res.VerifyURL)
	_ = sse.PatchElementTempl(webtempl.RegisterDoneFragment(email))
}

func registerErrMsg(err error) string {
	switch {
	case errors.Is(err, ErrEmailTaken):
		return "Email is already registered"
	case errors.Is(err, ErrWeakPassword):
		return "Password must be at least 8 characters"
	case errors.Is(err, ErrInviteInvalid):
		return "Invite code is invalid or expired"
	case errors.Is(err, ErrInviteUsed):
		return "Invite code has already been used"
	}
	return ""
}

// --- verify ---

func (h *Handler) GetVerify(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		_ = webtempl.VerifyPage("Missing token", false).Render(r.Context(), w)
		return
	}
	res, err := h.Svc.Verify(r.Context(), token, h.CommunityID)
	if err != nil {
		_ = webtempl.VerifyPage("Verification link is invalid or expired", false).Render(r.Context(), w)
		return
	}
	PutLogin(r.Context(), h.Sessions, res.UserID, res.CommunityID)
	_ = webtempl.VerifyPage("Account verified. You're signed in.", true).Render(r.Context(), w)
}

// --- login ---

func (h *Handler) GetLogin(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.LoginPage().Render(r.Context(), w)
}

type loginSignals struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *Handler) PostLogin(w http.ResponseWriter, r *http.Request) {
	var in loginSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	email := strings.TrimSpace(in.Email)
	if email == "" || in.Password == "" {
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment("Email and password required"))
		return
	}
	res, err := h.Svc.Login(r.Context(), email, in.Password, h.CommunityID)
	if err != nil {
		msg := "Something went wrong"
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			msg = "Invalid email or password"
		case errors.Is(err, ErrNotVerified):
			msg = "Please verify your email first"
		case errors.Is(err, ErrBanned):
			msg = "Your account is banned"
		}
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment(msg))
		return
	}
	PutLogin(r.Context(), h.Sessions, res.User.ID, res.Membership.CommunityID)
	_ = sse.Redirect("/")
}

// --- logout ---

func (h *Handler) PostLogout(w http.ResponseWriter, r *http.Request) {
	_ = Logout(r.Context(), h.Sessions)
	sse := datastar.NewSSE(w, r)
	_ = sse.Redirect("/login")
}

// --- profile ---

func (h *Handler) GetProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	_ = webtempl.ProfilePage(h.Viewer(r), id.Membership.DisplayName, id.Membership.AvatarURL).Render(r.Context(), w)
}

type profileSignals struct {
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
}

func (h *Handler) PostProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in profileSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := datastar.NewSSE(w, r)
	displayName := strings.TrimSpace(in.DisplayName)
	avatarURL := strings.TrimSpace(in.AvatarURL)
	if displayName == "" || len(displayName) > 40 {
		_ = sse.PatchElementTempl(webtempl.ProfileStatusFragment("Display name must be 1–40 characters", false))
		return
	}
	if err := h.Repo.UpdateMembershipProfile(r.Context(), id.Membership.ID, displayName, avatarURL); err != nil {
		h.Log.Error("update profile", "err", err)
		_ = sse.PatchElementTempl(webtempl.ProfileStatusFragment("Could not save", false))
		return
	}
	_ = sse.PatchElementTempl(webtempl.ProfileStatusFragment("Saved.", true))
}
