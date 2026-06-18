package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/render"

	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// commitSession flushes the scs session cookie to w *before* datastar.NewSSE
// flushes the response.
//
// scs's LoadAndSave middleware wraps w in a sessionResponseWriter that commits
// the session and writes Set-Cookie on the first WriteHeader/Write call. But
// datastar.NewSSE calls http.NewResponseController(w).Flush() — which unwraps
// past scs via sessionResponseWriter.Unwrap() and flushes the *underlying*
// http.ResponseWriter directly. The scs wrapper never sees the write so
// Set-Cookie is never sent. Callers must commit explicitly before NewSSE.
func commitSession(sm *scs.SessionManager, w http.ResponseWriter, r *http.Request) {
	switch sm.Status(r.Context()) {
	case scs.Modified:
		token, expiry, err := sm.Commit(r.Context())
		if err != nil {
			return
		}
		sm.WriteSessionCookie(r.Context(), w, token, expiry)
	case scs.Destroyed:
		sm.WriteSessionCookie(r.Context(), w, "", time.Time{})
	}
}

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
	r.Get("/register-as-admin", h.GetRegisterAsAdmin)
	r.Post("/register-as-admin", h.PostRegisterAsAdmin)
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
	// Zero-users install: bootstrap the first admin without an invite code.
	if n, err := h.Repo.CountUsers(r.Context()); err == nil && n == 0 {
		http.Redirect(w, r, "/register-as-admin", http.StatusSeeOther)
		return
	}
	_ = webtempl.RegisterPage(h.Svc.OpenRegistration).Render(r.Context(), w)
}

// --- register-as-admin (zero-users bootstrap) ---

func (h *Handler) GetRegisterAsAdmin(w http.ResponseWriter, r *http.Request) {
	n, err := h.Repo.CountUsers(r.Context())
	if err == nil && n > 0 {
		http.Redirect(w, r, "/register", http.StatusSeeOther)
		return
	}
	_ = webtempl.RegisterAsAdminPage(h.CommunityName).Render(r.Context(), w)
}

type registerAdminSignals struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

func (h *Handler) PostRegisterAsAdmin(w http.ResponseWriter, r *http.Request) {
	var in registerAdminSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	email := strings.TrimSpace(in.Email)
	if email == "" || in.Password == "" {
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment("Email and password required"))
		return
	}
	res, err := h.Svc.RegisterAsAdmin(r.Context(), RegisterAsAdminInput{
		Email:       email,
		Password:    in.Password,
		DisplayName: in.DisplayName,
	}, h.CommunityID)
	if err != nil {
		msg := registerErrMsg(err)
		if msg == "" {
			if errors.Is(err, ErrInviteInvalid) {
				msg = "An admin already exists — use the regular registration form"
			} else {
				h.Log.Error("register-as-admin failed", "err", err)
				msg = "Something went wrong"
			}
		}
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment(msg))
		return
	}
	PutLogin(r.Context(), h.Sessions, res.UserID, res.CommunityID)
	commitSession(h.Sessions, w, r)
	_ = sse.Redirect("/")
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
	email := strings.TrimSpace(in.Email)
	invite := strings.TrimSpace(strings.ToUpper(in.InviteCode))
	if email == "" || in.Password == "" {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment("Email and password required"))
		return
	}
	if invite == "" && !h.Svc.OpenRegistration {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment("Invite code required"))
		return
	}
	res, err := h.Svc.Register(r.Context(), RegisterInput{Email: email, Password: in.Password, InviteCode: invite})
	if err != nil {
		sse := render.NewSSE(w, r)
		msg := registerErrMsg(err)
		if msg == "" {
			h.Log.Error("register failed", "err", err)
			msg = "Something went wrong"
		}
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment(msg))
		return
	}
	// AUTO_VERIFY_EMAIL skipped the verification step — the user is already
	// active and a member, so sign them straight in. Commit the session BEFORE
	// NewSSE (§4.4: datastar's flush bypasses scs's Set-Cookie hook otherwise).
	if res.AutoVerified {
		h.Log.Info("user registered (auto-verified)", "user_id", res.UserID)
		PutLogin(r.Context(), h.Sessions, res.UserID, res.CommunityID)
		commitSession(h.Sessions, w, r)
		sse := render.NewSSE(w, r)
		_ = sse.Redirect("/")
		return
	}
	h.Log.Info("user registered", "user_id", res.UserID, "verify_url", res.VerifyURL)
	sse := render.NewSSE(w, r)
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
	case errors.Is(err, ErrInviteRequired):
		return "Invite code required"
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
	email := strings.TrimSpace(in.Email)
	if email == "" || in.Password == "" {
		sse := render.NewSSE(w, r)
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
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment(msg))
		return
	}
	// Mutate session and flush Set-Cookie BEFORE NewSSE — NewSSE.Flush() unwraps
	// past scs's writer, so any cookie write after it is dropped.
	PutLogin(r.Context(), h.Sessions, res.User.ID, res.Membership.CommunityID)
	commitSession(h.Sessions, w, r)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/")
}

// --- step-1: email check ---

type loginCheckSignals struct {
	Email string `json:"email"`
}

// PostLoginCheck advances the two-step login from "enter email" to
// "pick a method". We never reveal whether the email maps to an
// account here — pretending the user exists keeps account enumeration
// off the table; the real check happens at password submit or magic-
// link consume time.
func (h *Handler) PostLoginCheck(w http.ResponseWriter, r *http.Request) {
	var in loginCheckSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	email := strings.TrimSpace(in.Email)
	if email == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("auth-error", "Enter your email"))
		return
	}
	_ = sse.PatchElementTempl(webtempl.LoginStep2(email))
}

// PostLoginBack rewinds the card to step 1, letting the user correct
// a mistyped address without losing the email signal.
func (h *Handler) PostLoginBack(w http.ResponseWriter, r *http.Request) {
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.LoginStep1())
}

// PostLoginMagic mails a one-shot sign-in link to the address from the
// email signal. Always renders "check your email" — including when the
// address is unknown — so the response shape can't be used to probe
// membership.
func (h *Handler) PostLoginMagic(w http.ResponseWriter, r *http.Request) {
	var in loginCheckSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	email := strings.TrimSpace(in.Email)
	if email == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("auth-error", "Enter your email first"))
		return
	}
	if err := h.Svc.IssueMagicLink(r.Context(), email); err != nil {
		h.Log.Error("issue magic link", "err", err)
		// fall through to the same "check your email" page — the user
		// can't act on the failure and we won't expose it.
	}
	_ = sse.PatchElementTempl(webtempl.LoginMagicSent(email))
}

// GetLoginMagic consumes a magic-login token, mints the session and
// redirects home. Renders the verify-page error look when the token
// is missing or burnt.
func (h *Handler) GetLoginMagic(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		_ = webtempl.VerifyPage("Missing sign-in token", false).Render(r.Context(), w)
		return
	}
	res, err := h.Svc.ConsumeMagicLink(r.Context(), token, h.CommunityID)
	if err != nil {
		_ = webtempl.VerifyPage("Sign-in link is invalid or expired", false).Render(r.Context(), w)
		return
	}
	PutLogin(r.Context(), h.Sessions, res.User.ID, res.Membership.CommunityID)
	commitSession(h.Sessions, w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// --- logout ---

func (h *Handler) PostLogout(w http.ResponseWriter, r *http.Request) {
	_ = Logout(r.Context(), h.Sessions)
	commitSession(h.Sessions, w, r)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/login")
}

// --- pending ---

// GetPending renders the "your join request is in the queue" page. Mounted on
// /pending; the RequireApproved middleware redirects unapproved members here.
func (h *Handler) GetPending(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.PendingPage(h.Viewer(r)).Render(r.Context(), w)
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
	sse := render.NewSSE(w, r)
	displayName := strings.TrimSpace(in.DisplayName)
	avatarURL := strings.TrimSpace(in.AvatarURL)
	if displayName == "" || len(displayName) > 40 {
		_ = sse.PatchElementTempl(webtempl.ProfileStatusFragment("Display name must be 1–40 characters", false))
		return
	}
	// Update every membership the user holds — the profile editor is
	// "you", not "you in this community". Without this, only the current
	// community got the new name and others kept the email-localpart
	// fallback that admin.PostAddMember assigns on invite.
	if err := h.Repo.UpdateAllMembershipProfiles(r.Context(), id.User.ID, displayName, avatarURL); err != nil {
		h.Log.Error("update profile", "err", err)
		_ = sse.PatchElementTempl(webtempl.ProfileStatusFragment("Could not save", false))
		return
	}
	_ = sse.PatchElementTempl(webtempl.ProfileStatusFragment("Saved across every community.", true))
}
