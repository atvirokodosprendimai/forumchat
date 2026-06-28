package auth

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"

	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// GetOAuthBegin starts the provider redirect dance. The {provider} URL segment
// selects the goth provider. If gothic finds a completed session already (rare —
// e.g. a refresh of the begin URL after a callback), it signs in directly.
func (h *Handler) GetOAuthBegin(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	r = gothic.GetContextWithProvider(r, provider)
	if gu, err := gothic.CompleteUserAuth(w, r); err == nil {
		h.finishOAuthLogin(w, r, provider, gu)
		return
	}
	gothic.BeginAuthHandler(w, r)
}

// GetOAuthCallback completes the provider round trip, resolves the user and
// mints the scs session. These are plain redirects (not SSE), so the
// commitSession + http.Redirect order behaves normally — the §4.4 datastar
// flush bug does not apply here.
func (h *Handler) GetOAuthCallback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	r = gothic.GetContextWithProvider(r, provider)
	gu, err := gothic.CompleteUserAuth(w, r)
	if err != nil {
		h.Log.Warn("oauth callback failed", "provider", provider, "err", err)
		_ = webtempl.VerifyPage("Sign-in with "+providerLabel(provider)+" failed or was cancelled.", false).Render(r.Context(), w)
		return
	}
	h.finishOAuthLogin(w, r, provider, gu)
}

func (h *Handler) finishOAuthLogin(w http.ResponseWriter, r *http.Request, provider string, gu goth.User) {
	// GitHub (and some providers) leave Name empty — fall back to the login /
	// nickname so the membership display name isn't just the email localpart.
	name := gu.Name
	if name == "" {
		name = gu.NickName
	}
	res, err := h.Svc.UpsertOAuthUser(r.Context(), OAuthInput{
		Provider:       provider,
		ProviderUserID: gu.UserID,
		Email:          gu.Email,
		Name:           name,
		AvatarURL:      gu.AvatarURL,
		EmailVerified:  providerEmailVerified(provider, gu),
	})
	if err != nil {
		label := providerLabel(provider)
		msg := "Could not sign you in."
		switch {
		case errors.Is(err, ErrOAuthNoEmail):
			msg = "Your " + label + " account didn't share an email address, so we can't sign you in."
		case errors.Is(err, ErrOAuthNoAccount):
			msg = "No account is registered for that email. Ask an admin for an invite, then sign in with " + label + " once you're a member."
		case errors.Is(err, ErrOAuthEmailUnverified):
			msg = "An account already exists for that email, but " + label + " didn't verify you own it. Please sign in with your password instead."
		case errors.Is(err, ErrOAuthAgeGate):
			msg = "New accounts can't be created with " + label + " on this site. Please register with the form, where you can confirm your age."
		case errors.Is(err, ErrBanned):
			msg = "Your account is disabled."
		default:
			h.Log.Error("oauth upsert", "provider", provider, "err", err)
		}
		_ = webtempl.VerifyPage(msg, false).Render(r.Context(), w)
		return
	}
	PutLogin(r.Context(), h.Sessions, res.User.ID, res.Membership.CommunityID)
	commitSession(h.Sessions, w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// oauthButtons maps the handler's enabled providers to the leaf-package view
// model the templates render (web/templ can't import auth — §4.13).
func (h *Handler) oauthButtons() []webtempl.OAuthButton {
	if len(h.OAuthProviders) == 0 {
		return nil
	}
	btns := make([]webtempl.OAuthButton, 0, len(h.OAuthProviders))
	for _, p := range h.OAuthProviders {
		btns = append(btns, webtempl.OAuthButton{Provider: p.Name, Label: p.Label})
	}
	return btns
}

// providerEmailVerified reports whether the provider proved the user owns the
// returned email — the gate for auto-linking to an existing local account.
//   - github: goth fetches only *verified* primary emails (user:email scope),
//     so a returned email is verified.
//   - google: the OIDC userinfo carries email_verified / verified_email.
//   - facebook (and any future provider): no verified-email guarantee → treat
//     as unverified so it can't take over an existing password account.
func providerEmailVerified(provider string, gu goth.User) bool {
	switch provider {
	case "github":
		return true
	case "google":
		for _, k := range []string{"email_verified", "verified_email"} {
			switch v := gu.RawData[k].(type) {
			case bool:
				return v
			case string:
				return v == "true"
			}
		}
		return false
	default:
		return false
	}
}

// providerLabel returns the human label for a provider key, falling back to the
// key itself for providers without a registered label.
func providerLabel(name string) string {
	switch name {
	case "google":
		return "Google"
	case "facebook":
		return "Facebook"
	case "github":
		return "GitHub"
	default:
		return name
	}
}
