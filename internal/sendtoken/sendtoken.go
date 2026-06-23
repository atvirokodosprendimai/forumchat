// Package sendtoken issues and validates short-lived, per-user "send tokens".
//
// A token is HMAC(key, userID, windowIndex) truncated to 16 bytes, where
// windowIndex = unix_seconds / WindowSecs. The layout's long-lived SSE stream
// patches the current-window token into the client signal bag once on connect
// and again on its keep-alive tick; every member-write POST carries it back and
// the server accepts it only if it matches one of the last AcceptWindows
// windows (~3 minutes of validity).
//
// Properties (and limits):
//   - Stateless: derived from a stable key, so it survives a process restart
//     with no DB and is identical across a user's tabs — no per-session store,
//     no FIFO eviction, no clock-skew bookkeeping.
//   - Liveness / CSRF hardening: the current token can ONLY be learned by
//     reading the authenticated SSE stream (it can't be computed without the
//     key), so a client that never subscribes — or a cross-site forge — is
//     rejected.
//   - NOT bot-proof: SSE is scriptable, so a determined scraper can read the
//     stream and replay the token within its window. This is a defense-in-depth
//     layer that pairs with rate-limiting, not a CAPTCHA substitute.
package sendtoken

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"time"
)

const (
	// WindowSecs is the token rotation period in seconds.
	WindowSecs = 60
	// AcceptWindows is how many windows (including the current one) validate, so
	// a token minted at the start of a window is honoured for ~AcceptWindows
	// minutes — long enough that an in-flight send across a rotation boundary,
	// or a client that briefly missed a patch, is never wrongly rejected.
	AcceptWindows = 3
)

// Signer issues and validates send tokens. Construct with New.
type Signer struct {
	key []byte
	now func() time.Time
}

// New returns a Signer keyed by secret (reuse an existing app secret, e.g.
// SESSION_KEY). Callers may pass a nil *Signer to a handler to disable
// enforcement; the methods are only called on a non-nil Signer.
func New(secret string) *Signer {
	return &Signer{key: []byte(secret), now: time.Now}
}

func (s *Signer) window() int64 { return s.now().Unix() / WindowSecs }

func (s *Signer) tokenFor(userID string, win int64) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(userID))
	mac.Write([]byte{0x1f}) // unit separator: unambiguous userID|window boundary
	mac.Write([]byte(strconv.FormatInt(win, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

// Issue returns userID's current-window token (the value patched to the
// client). Empty userID yields an empty token.
func (s *Signer) Issue(userID string) string {
	if userID == "" {
		return ""
	}
	return s.tokenFor(userID, s.window())
}

// Valid reports whether token matches userID's current or any of the
// AcceptWindows-1 immediately-preceding windows. Constant-time per comparison.
func (s *Signer) Valid(userID, token string) bool {
	if userID == "" || token == "" {
		return false
	}
	cur := s.window()
	ok := false
	for i := int64(0); i < AcceptWindows; i++ {
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.tokenFor(userID, cur-i))) == 1 {
			ok = true
		}
	}
	return ok
}
