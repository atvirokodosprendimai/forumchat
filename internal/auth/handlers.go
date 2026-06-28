package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
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
// CommitSession writes the scs session cookie to w. Call it BEFORE
// datastar.NewSSE in any handler that mutates session state — NewSSE's Flush
// bypasses scs's own Set-Cookie hook, so the cookie is otherwise dropped
// (§4.4). Exported for handlers outside this package (e.g. internal/invites).
func CommitSession(sm *scs.SessionManager, w http.ResponseWriter, r *http.Request) {
	commitSession(sm, w, r)
}

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
	// RegisterMinAge mirrors config.RegisterMinAge: when > 0 the register form
	// shows a required self-attestation checkbox and PostRegister rejects an
	// unticked signup. 0 = gate off. Wired in main.go.
	RegisterMinAge int
	// OAuthProviders are the enabled social-login providers (empty = OAuth off).
	// Rendered as "Continue with …" buttons on the login + register pages.
	OAuthProviders []OAuthProvider
	// MyCommunities lists the communities the caller can leave — every approved,
	// non-banned membership — so the leave-community picker can offer all of them
	// (a member may belong to many; the global /profile page must not force the
	// one the session happens to be bound to). currentID flags and pre-selects
	// the session community. Also reused after a leave to rebind the session or
	// re-render the picker. Wired in main.go to community.Repo.ListForUser (a
	// closure so auth doesn't import community).
	MyCommunities func(ctx context.Context, userID, currentID string) []webtempl.LeaveCommunityRow
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
	_ = webtempl.RegisterPage(h.Svc.OpenRegistration, h.RegisterMinAge, h.oauthButtons()).Render(r.Context(), w)
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
	email := strings.TrimSpace(in.Email)
	if email == "" || in.Password == "" {
		sse := render.NewSSE(w, r)
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
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment(msg))
		return
	}
	// Success: commit the session cookie BEFORE NewSSE flushes the response,
	// else datastar's Flush bypasses scs's Set-Cookie hook and the login is
	// silently dropped (§4.4). RenewToken (in PutLogin) makes this load-bearing.
	PutLogin(r.Context(), h.Sessions, res.UserID, res.CommunityID)
	commitSession(h.Sessions, w, r)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/")
}

type registerSignals struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	InviteCode   string `json:"invite_code"`
	AgeConfirmed bool   `json:"age_confirmed"`
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
	// Age gate: when configured, the self-attestation checkbox is required. The
	// boolean IS the attestation, so we re-check it server-side here rather than
	// trusting the client-side disabled button (which a crafted request skips).
	if h.RegisterMinAge > 0 && !in.AgeConfirmed {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.RegisterErrorFragment(
			"You must confirm you are at least " + strconv.Itoa(h.RegisterMinAge) + " years old to register."))
		return
	}
	res, err := h.Svc.Register(r.Context(), RegisterInput{Email: email, Password: in.Password, InviteCode: invite})
	if err != nil {
		sse := render.NewSSE(w, r)
		// Don't leak whether an email is already registered (FIX1 L3): a taken
		// email gets the SAME "check your email" terminal page as a fresh signup,
		// so registration can't be used to enumerate accounts (the login + magic
		// flows are already non-revealing).
		if errors.Is(err, ErrEmailTaken) {
			_ = sse.PatchElementTempl(webtempl.RegisterDoneFragment(email))
			return
		}
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
	_ = webtempl.LoginPage(h.oauthButtons()).Render(r.Context(), w)
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
	_ = sse.PatchElementTempl(webtempl.LoginStep1(h.oauthButtons()))
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
	// The leave-community picker lists every community the member belongs to (a
	// member may be in many), with the session one pre-selected.
	var leaveRows []webtempl.LeaveCommunityRow
	if h.MyCommunities != nil {
		leaveRows = h.MyCommunities(r.Context(), id.User.ID, id.Membership.CommunityID)
	}
	// hasPassword decides whether the password card asks for the current one.
	// OAuth-only users (sentinel hash) are setting a first password, not changing.
	_ = webtempl.ProfilePage(h.Viewer(r), id.Membership.DisplayName, id.Membership.AvatarURL, id.User.HasPassword(), leaveRows).Render(r.Context(), w)
}

type leaveSignals struct {
	// CommunityID is which of the caller's OWN communities to leave (picker
	// value). Trusted only as "one of MY memberships": Service.LeaveCommunity
	// scopes the delete to (sessionUserID, CommunityID), so a forged id can at
	// most leave a community the caller isn't in → ErrNotAMember. No IDOR.
	CommunityID string `json:"leave_community_id"`
}

// PostLeaveCommunity removes the caller's own membership in the community they
// picked. If that was the community their session is bound to, it rebinds the
// session to another of their communities (or signs out if none remain) and
// redirects; otherwise the session is untouched and the picker re-renders in
// place. Single-step (no email confirm) because leaving is reversible by
// rejoining. The last-admin orphan guard lives in Service.LeaveCommunity.
func (h *Handler) PostLeaveCommunity(w http.ResponseWriter, r *http.Request) {
	id, ok := FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in leaveSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Default to the session community so a single-community member (whose picker
	// may not have seeded a value) still works.
	leaveCID := strings.TrimSpace(in.CommunityID)
	if leaveCID == "" {
		leaveCID = id.Membership.CommunityID
	}

	if err := h.Svc.LeaveCommunity(r.Context(), id.User.ID, leaveCID); err != nil {
		sse := render.NewSSE(w, r)
		var msg string
		switch {
		case errors.Is(err, ErrLeaveLastAdmin):
			msg = "You're the last admin there — promote another admin or delete the community before leaving."
		case errors.Is(err, ErrNotAMember):
			msg = "You're not a member of that community."
		default:
			h.Log.Error("leave community", "user_id", id.User.ID, "community_id", leaveCID, "err", err)
			msg = "Couldn't leave — please try again."
		}
		_ = sse.PatchElementTempl(webtempl.LeaveCommunityStatusFragment(msg, false))
		return
	}
	h.Log.Info("member left community", "user_id", id.User.ID, "community_id", leaveCID)

	// Leaving a community OTHER than the session one leaves the session valid —
	// just re-render the picker (now without it) plus a success line. No redirect.
	if leaveCID != id.Membership.CommunityID {
		var rows []webtempl.LeaveCommunityRow
		if h.MyCommunities != nil {
			rows = h.MyCommunities(r.Context(), id.User.ID, id.Membership.CommunityID)
		}
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.LeaveCommunityCard(rows))
		_ = sse.PatchElementTempl(webtempl.LeaveCommunityStatusFragment("You've left the community.", true))
		return
	}

	// They left the community the session is bound to: rebind to another they
	// still belong to (so a multi-community member isn't signed out entirely), or
	// sign out. Session mutation must be committed BEFORE render.NewSSE (§4.4).
	var remaining []webtempl.LeaveCommunityRow
	if h.MyCommunities != nil {
		remaining = h.MyCommunities(r.Context(), id.User.ID, "")
	}
	if len(remaining) > 0 {
		PutLogin(r.Context(), h.Sessions, id.User.ID, remaining[0].CommunityID)
		commitSession(h.Sessions, w, r)
		sse := render.NewSSE(w, r)
		_ = sse.Redirect("/c/" + remaining[0].Slug + "/chat")
		return
	}
	_ = Logout(r.Context(), h.Sessions)
	commitSession(h.Sessions, w, r)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/")
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

type passwordSignals struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	ConfirmPassword string `json:"new_password_confirm"`
}

// PostPassword lets a signed-in user change (or, for an OAuth-only account, set)
// their password. A user who already has a usable password must supply the
// current one; an OAuth-only user (sentinel hash) is setting a first password
// and is not asked for one. No session state is mutated, so the §4.4
// commitSession dance does not apply.
func (h *Handler) PostPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in passwordSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	// fail renders an error fragment via a fresh SSE. It's lazy (creates the SSE
	// itself) so the success path can mutate the session and commit its cookie
	// BEFORE opening the SSE — datastar's NewSSE flush bypasses scs's Set-Cookie
	// hook (§4.4), so RenewToken's new token would otherwise never reach the
	// browser and the user would be logged out by their own password change.
	fail := func(msg string) {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.PasswordStatusFragment(msg, false))
	}

	// Re-read the user so the password check is against the live hash, not stale
	// session state.
	user, err := h.Repo.UserByID(r.Context(), id.User.ID)
	if err != nil {
		h.Log.Error("change password: load user", "err", err)
		fail("Could not load your account")
		return
	}
	// Users with a real password must prove they know it; OAuth-only users are
	// setting a first password and skip this gate.
	if user.HasPassword() && !CheckPassword(user.PasswordHash, in.CurrentPassword) {
		fail("Current password is incorrect")
		return
	}
	if in.NewPassword != in.ConfirmPassword {
		fail("New passwords do not match")
		return
	}
	hash, err := HashPassword(in.NewPassword)
	if err != nil {
		if errors.Is(err, ErrWeakPassword) {
			fail("Password must be at least 8 characters")
			return
		}
		h.Log.Error("change password: hash", "err", err)
		fail("Could not update password")
		return
	}
	if err := h.Repo.UpdatePassword(r.Context(), user.ID, hash); err != nil {
		h.Log.Error("change password: persist", "err", err)
		fail("Could not update password")
		return
	}
	// Rotate the session token on password change (FIX1 M4) — a session-fixation
	// defense that also re-mints the current device's cookie. commitSession must
	// run BEFORE NewSSE so the new Set-Cookie survives datastar's flush (§4.4).
	// Guarded on Sessions: in production it's always wired behind LoadAndSave;
	// the nil guard keeps direct-handler unit tests (no scs context) from
	// panicking inside RenewToken.
	if h.Sessions != nil {
		_ = h.Sessions.RenewToken(r.Context())
		commitSession(h.Sessions, w, r)
	}

	sse := render.NewSSE(w, r)
	// The card flips to "has a password" — a freshly-set OAuth password now needs
	// the current one for the next change. Re-render the whole card so the
	// current-password field appears and the inputs clear.
	_ = sse.PatchElementTempl(webtempl.PasswordCard(true, true))
	_ = sse.PatchSignals([]byte(`{"current_password":"","new_password":"","new_password_confirm":""}`))
}

// --- account erasure (self-serve delete) ---

const deleteTokenPurpose = "account_delete"

type deletePasswordSignals struct {
	Password string `json:"delete_password"`
}

// soleOwnerMessage turns the blocking communities into a friendly sentence.
func soleOwnerMessage(blockers []CommunityRef) string {
	names := make([]string, len(blockers))
	for i, b := range blockers {
		names[i] = b.Name
	}
	return "You're the only owner of " + strings.Join(names, ", ") +
		". Transfer ownership or delete " +
		map[bool]string{true: "it", false: "them"}[len(blockers) == 1] +
		" before deleting your account."
}

// PostDeleteStart is step 1 of erasure: prove the password (OAuth-only accounts
// skip it — the emailed link is the proof), block early if the user solely owns
// a community with other members, then email a one-time confirmation link.
// Nothing is deleted here.
func (h *Handler) PostDeleteStart(w http.ResponseWriter, r *http.Request) {
	id, ok := FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in deletePasswordSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	fail := func(msg string) {
		_ = sse.PatchElementTempl(webtempl.DeleteAccountStatusFragment(msg, false))
	}
	user, err := h.Repo.UserByID(r.Context(), id.User.ID)
	if err != nil {
		h.Log.Error("delete start: load user", "err", err)
		fail("Could not load your account")
		return
	}
	if user.HasPassword() && !CheckPassword(user.PasswordHash, in.Password) {
		fail("Password is incorrect")
		return
	}
	// Early sole-owner guard so the user sees it before any email is sent.
	blockers, err := h.Svc.CheckDeletable(r.Context(), user.ID)
	if err != nil {
		h.Log.Error("delete start: check deletable", "err", err)
		fail("Something went wrong")
		return
	}
	if len(blockers) > 0 {
		fail(soleOwnerMessage(blockers))
		return
	}
	if err := h.Svc.IssueDeletionLink(r.Context(), user.ID, user.Email); err != nil {
		h.Log.Error("delete start: issue link", "err", err)
		fail("Could not send the confirmation email — please try again")
		return
	}
	_ = sse.PatchElementTempl(webtempl.DeleteAccountStatusFragment(
		"Check your email for a confirmation link. It expires in 30 minutes.", true))
}

// GetDeleteConfirm renders the terminal confirmation page for a valid deletion
// link. The token is validated (not consumed) so a stale/expired link shows the
// error look instead of the confirm button. Public + token-gated — the token is
// the authorization (it was emailed to the account behind a password gate).
func (h *Handler) GetDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		_ = webtempl.VerifyPage("Missing confirmation token", false).Render(r.Context(), w)
		return
	}
	if _, err := h.Repo.PeekVerificationToken(r.Context(), token, deleteTokenPurpose); err != nil {
		_ = webtempl.VerifyPage("This deletion link is invalid or expired", false).Render(r.Context(), w)
		return
	}
	_ = webtempl.DeleteAccountConfirmPage(h.Viewer(r), token).Render(r.Context(), w)
}

type deleteConfirmSignals struct {
	Token string `json:"delete_token"`
}

// PostDeleteConfirm burns the deletion token and runs the irreversible erasure
// for the token's user, then destroys the session and redirects to the goodbye
// page. Token-gated (no session required — clicking the email in any browser
// works), matching the magic-link trust model.
func (h *Handler) PostDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	var in deleteConfirmSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	vt, err := h.Repo.ConsumeVerificationToken(r.Context(), strings.TrimSpace(in.Token), deleteTokenPurpose)
	if err != nil {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("delete-confirm-error", "This deletion link is invalid or expired."))
		return
	}
	if err := h.Svc.DeleteAccount(r.Context(), vt.UserID); err != nil {
		sse := render.NewSSE(w, r)
		var soleErr *SoleOwnerError
		if errors.As(err, &soleErr) {
			_ = sse.PatchElementTempl(webtempl.ErrorFragment("delete-confirm-error", soleOwnerMessage(soleErr.Blockers)))
			return
		}
		h.Log.Error("delete account", "user_id", vt.UserID, "err", err)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("delete-confirm-error", "Deletion failed — please try again."))
		return
	}
	h.Log.Info("account erased (self-serve)", "user_id", vt.UserID)
	// Destroy the session (the row is now a disabled tombstone) and flush the
	// cookie BEFORE NewSSE per §4.4.
	_ = Logout(r.Context(), h.Sessions)
	commitSession(h.Sessions, w, r)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/goodbye")
}

// GetGoodbye renders the post-erasure confirmation page.
func (h *Handler) GetGoodbye(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.GoodbyePage().Render(r.Context(), w)
}
