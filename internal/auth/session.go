package auth

import (
	"context"
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
)

const (
	sessionKeyUserID      = "user_id"
	sessionKeyCommunityID = "community_id"
)

func NewSessionManager(maxAge time.Duration, secure bool) *scs.SessionManager {
	sm := scs.New()
	sm.Lifetime = maxAge
	sm.IdleTimeout = maxAge
	sm.Cookie.Name = "forumchat_session"
	sm.Cookie.HttpOnly = true
	sm.Cookie.Secure = secure
	sm.Cookie.SameSite = http.SameSiteLaxMode
	sm.Cookie.Path = "/"
	return sm
}

func PutLogin(ctx context.Context, sm *scs.SessionManager, userID, communityID string) {
	// Rotate the session token at the privilege boundary (anonymous →
	// authenticated) to defeat session fixation: a token an attacker planted
	// pre-login can't carry over into the authenticated session.
	_ = sm.RenewToken(ctx)
	sm.Put(ctx, sessionKeyUserID, userID)
	sm.Put(ctx, sessionKeyCommunityID, communityID)
}

func CurrentUserID(ctx context.Context, sm *scs.SessionManager) string {
	return sm.GetString(ctx, sessionKeyUserID)
}

func CurrentCommunityID(ctx context.Context, sm *scs.SessionManager) string {
	return sm.GetString(ctx, sessionKeyCommunityID)
}

func Logout(ctx context.Context, sm *scs.SessionManager) error {
	return sm.Destroy(ctx)
}
