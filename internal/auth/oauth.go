package auth

import (
	"github.com/gorilla/sessions"
	"github.com/markbates/goth"
	"github.com/markbates/goth/gothic"
	"github.com/markbates/goth/providers/facebook"
	"github.com/markbates/goth/providers/google"
)

// oauthSentinelHash is stored as password_hash for accounts that only ever sign
// in through an OAuth provider. It is not a valid bcrypt hash, so CheckPassword
// always returns false against it — password login stays disabled for these
// users (magic-link and OAuth remain open).
const oauthSentinelHash = "oauth-no-password"

// OAuthProvider is an enabled social-login provider surfaced to the templates
// as a "Continue with <Label>" button.
type OAuthProvider struct {
	Name  string // goth provider key, also the {provider} URL segment ("google")
	Label string // human label ("Google")
}

// OAuthConfig carries the per-provider credentials plus the public base URL the
// callback URLs are built from.
type OAuthConfig struct {
	BaseURL              string
	Secure               bool
	SessionKey           string
	GoogleClientID       string
	GoogleClientSecret   string
	FacebookClientID     string
	FacebookClientSecret string
}

// SetupOAuth registers every goth provider that has credentials and points
// gothic's transient session store at a cookie store keyed off the app session
// key. It returns the enabled providers (for rendering buttons); an empty slice
// means OAuth is off — the caller should skip mounting the /auth routes.
//
// The gothic cookie only lives for the redirect→callback round trip (it holds
// the OAuth state nonce); the real, long-lived session is the scs one minted
// after a successful callback.
func SetupOAuth(cfg OAuthConfig) []OAuthProvider {
	var provs []goth.Provider
	var enabled []OAuthProvider

	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		provs = append(provs, google.New(
			cfg.GoogleClientID, cfg.GoogleClientSecret,
			cfg.BaseURL+"/auth/google/callback", "email", "profile"))
		enabled = append(enabled, OAuthProvider{Name: "google", Label: "Google"})
	}
	if cfg.FacebookClientID != "" && cfg.FacebookClientSecret != "" {
		provs = append(provs, facebook.New(
			cfg.FacebookClientID, cfg.FacebookClientSecret,
			cfg.BaseURL+"/auth/facebook/callback", "email"))
		enabled = append(enabled, OAuthProvider{Name: "facebook", Label: "Facebook"})
	}
	if len(provs) == 0 {
		return nil
	}
	goth.UseProviders(provs...)

	store := sessions.NewCookieStore([]byte(cfg.SessionKey))
	store.MaxAge(600) // 10 minutes — only spans the provider round trip
	store.Options.Path = "/"
	store.Options.HttpOnly = true
	store.Options.Secure = cfg.Secure
	gothic.Store = store

	return enabled
}
