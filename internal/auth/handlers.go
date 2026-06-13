package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"

	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Svc            *Service
	Repo           *Repo
	Sessions       *scs.SessionManager
	CommunityID    string
	CommunityName  string
	Log            *slog.Logger
}

func (h *Handler) Mount(r chiMux) {
	r.Get("/register", h.GetRegister)
	r.Post("/register", h.PostRegister)
	r.Get("/verify", h.GetVerify)
	r.Get("/login", h.GetLogin)
	r.Post("/login", h.PostLogin)
	r.Post("/logout", h.PostLogout)
}

type chiMux interface {
	Get(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
}

func (h *Handler) GetRegister(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.RegisterPage(webtempl.RegisterForm{}).Render(r.Context(), w)
}

func (h *Handler) PostRegister(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := webtempl.RegisterForm{
		Email:      strings.TrimSpace(r.PostFormValue("email")),
		InviteCode: strings.TrimSpace(strings.ToUpper(r.PostFormValue("invite_code"))),
	}
	password := r.PostFormValue("password")
	if form.Email == "" || password == "" || form.InviteCode == "" {
		form.Error = "All fields required"
		_ = webtempl.RegisterPage(form).Render(r.Context(), w)
		return
	}

	res, err := h.Svc.Register(r.Context(), RegisterInput{
		Email: form.Email, Password: password, InviteCode: form.InviteCode,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrEmailTaken):
			form.Error = "Email is already registered"
		case errors.Is(err, ErrWeakPassword):
			form.Error = "Password must be at least 8 characters"
		case errors.Is(err, ErrInviteInvalid):
			form.Error = "Invite code is invalid or expired"
		case errors.Is(err, ErrInviteUsed):
			form.Error = "Invite code has already been used"
		default:
			h.Log.Error("register failed", "err", err)
			form.Error = "Something went wrong"
		}
		_ = webtempl.RegisterPage(form).Render(r.Context(), w)
		return
	}
	h.Log.Info("user registered", "user_id", res.UserID, "verify_url", res.VerifyURL)
	_ = webtempl.RegisterDonePage(form.Email).Render(r.Context(), w)
}

func (h *Handler) GetVerify(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		_ = webtempl.VerifyPage("Missing token", false).Render(r.Context(), w)
		return
	}
	user, err := h.Svc.Verify(r.Context(), token, h.CommunityID)
	if err != nil {
		_ = webtempl.VerifyPage("Verification link is invalid or expired", false).Render(r.Context(), w)
		return
	}
	PutLogin(r.Context(), h.Sessions, user.UserID, user.CommunityID)
	_ = webtempl.VerifyPage("Account verified. You're signed in.", true).Render(r.Context(), w)
}

func (h *Handler) GetLogin(w http.ResponseWriter, r *http.Request) {
	form := webtempl.LoginForm{}
	if r.URL.Query().Get("banned") == "1" {
		form.Error = "Your account is banned."
	}
	_ = webtempl.LoginPage(form).Render(r.Context(), w)
}

func (h *Handler) PostLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(r.PostFormValue("email"))
	password := r.PostFormValue("password")
	form := webtempl.LoginForm{Email: email}

	res, err := h.Svc.Login(r.Context(), email, password, h.CommunityID)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			form.Error = "Invalid email or password"
		case errors.Is(err, ErrNotVerified):
			form.Error = "Please verify your email first"
		case errors.Is(err, ErrBanned):
			form.Error = "Your account is banned"
		default:
			h.Log.Error("login failed", "err", err)
			form.Error = "Something went wrong"
		}
		_ = webtempl.LoginPage(form).Render(r.Context(), w)
		return
	}
	PutLogin(r.Context(), h.Sessions, res.User.ID, res.Membership.CommunityID)
	next := r.URL.Query().Get("next")
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (h *Handler) PostLogout(w http.ResponseWriter, r *http.Request) {
	_ = Logout(r.Context(), h.Sessions)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

