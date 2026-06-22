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
	res, err := h.Svc.UpsertOAuthUser(r.Context(), OAuthInput{
		Provider:       provider,
		ProviderUserID: gu.UserID,
		Email:          gu.Email,
		Name:           gu.Name,
		AvatarURL:      gu.AvatarURL,
	})
	if err != nil {
		label := providerLabel(provider)
		msg := "Could not sign you in."
		switch {
		case errors.Is(err, ErrOAuthNoEmail):
			msg = "Your " + label + " account didn't share an email address, so we can't sign you in."
		case errors.Is(err, ErrOAuthNoAccount):
			msg = "No account is registered for that email. Ask an admin for an invite, then sign in with " + label + " once you're a member."
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

// providerLabel returns the human label for a provider key, falling back to the
// key itself for providers without a registered label.
func providerLabel(name string) string {
	switch name {
	case "google":
		return "Google"
	case "facebook":
		return "Facebook"
	default:
		return name
	}
}
