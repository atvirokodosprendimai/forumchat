package connectors

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// The two endpoints carry their credential differently because their transports
// differ (spec "Why signed-URL read + body-HMAC write"):
//
//   - The read stream is a long-lived EventSource that can't set custom headers,
//     so its credential rides IN THE URL as a server-minted signature
//     (StreamSig / VerifyStream) — a bearer capability, like the shared-signed
//     upload links.
//   - A send/action POST can carry a header, so it gets the stronger, tamper-proof
//     body signature (SignBody / VerifyBody), reusing the GitHub webhook scheme.
//
// One per-connector secret powers both; rotating it revokes both at once.

// StreamSig computes the hex HMAC-SHA256 over (id, "stream", expUnix) keyed by
// the connector secret. expUnix == 0 means a non-expiring URL (the signature is
// still bound to the id, so it can't be forged or reused for another connector).
func StreamSig(secret, id string, expUnix int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id))
	mac.Write([]byte{'\n'})
	mac.Write([]byte("stream"))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(expUnix, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyStream reports whether sig is a valid, unexpired stream signature for
// (secret, id, expUnix). Comparison is constant-time. A non-positive expUnix is
// treated as non-expiring. now is injected for testability.
func VerifyStream(secret, id, sig string, expUnix int64, now time.Time) bool {
	if expUnix > 0 && now.Unix() > expUnix {
		return false
	}
	want := StreamSig(secret, id, expUnix)
	return hmac.Equal([]byte(want), []byte(sig))
}

// SignBody returns the value an external worker puts in the X-Signature header
// for a /send (or other action) POST: "sha256=" + hex HMAC-SHA256(secret, body).
// Exposed so tests (and our own docs/snippet) sign the same way the worker must.
func SignBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifyBody reports whether the X-Signature header proves the raw body was
// produced by a holder of the connector secret. It accepts the "sha256=<hex>"
// form (mirroring webhooks.verifyGitHubSignature) and compares constant-time so
// a wrong-length or tampered signature leaks no timing information. A missing or
// malformed header is rejected.
func VerifyBody(secret string, body []byte, header string) bool {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), got)
}
