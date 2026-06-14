package lobbies

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
)

// guestCookieName is the cookie holding the lobby_id the visitor has
// already joined. Path-scoped to /lobby so it doesn't leak to host-side
// routes.
const guestCookieName = "fc_lobby"

// SetGuestCookie writes a signed cookie binding the browser to a lobby.
// The signature uses the global session secret so any forged cookie
// fails to verify — but recovery is cheap (guest just re-enters their
// name on the next visit).
func SetGuestCookie(w http.ResponseWriter, lobbyID, secret string) {
	value := signCookie(lobbyID, secret)
	http.SetCookie(w, &http.Cookie{
		Name:     guestCookieName,
		Value:    value,
		Path:     "/lobby/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60 * 60 * 24 * 30, // 30 days
	})
}

// ClearGuestCookie nulls out the cookie. Used when the guest is moved
// from one lobby to another via re-join with the same browser.
func ClearGuestCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:   guestCookieName,
		Value:  "",
		Path:   "/lobby/",
		MaxAge: -1,
	})
}

// GuestLobbyFromRequest returns the lobby_id stored in the signed
// cookie if it verifies. ok=false signals "no valid cookie" — the
// handler should fall back to the name-capture landing page.
func GuestLobbyFromRequest(r *http.Request, secret string) (string, bool) {
	c, err := r.Cookie(guestCookieName)
	if err != nil {
		return "", false
	}
	parts := strings.SplitN(c.Value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	lobbyID, sig := parts[0], parts[1]
	want := signature(lobbyID, secret)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return "", false
	}
	return lobbyID, true
}

func signCookie(lobbyID, secret string) string {
	return lobbyID + "." + signature(lobbyID, secret)
}

func signature(lobbyID, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(lobbyID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
