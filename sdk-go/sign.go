package connector

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// The connector wire authenticates its two transports differently, because they
// differ in what they can carry (server spec: "Why signed-URL read + body-HMAC
// write"):
//
//   - The read stream is a long-lived EventSource that cannot set custom
//     headers, so its credential rides IN THE URL as an HMAC signature bound to
//     the connector id + an expiry (streamSig).
//   - A send/action POST can carry a header, so it gets the stronger,
//     tamper-proof body HMAC in X-Signature (signBody).
//
// One per-connector secret powers both; rotating it (server-side) revokes both
// at once. These two functions are byte-for-byte copies of the server's
// internal/connectors/sign.go — duplicated, not shared, because the server
// package is internal/ and cannot be imported by an external consumer. If the
// server scheme ever changes, change it here too.

// streamSig computes the hex HMAC-SHA256 over (id, "stream", expUnix) keyed by
// the connector secret — the signature embedded in the stream URL's `sig` query
// param. expUnix == 0 means a non-expiring URL; the signature is still bound to
// the id, so it can be forged for neither another connector nor a later expiry.
func streamSig(secret, id string, expUnix int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(id))
	mac.Write([]byte{'\n'})
	mac.Write([]byte("stream"))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(expUnix, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// signBody returns the X-Signature header value for a signed POST (send and the
// moderation actions): "sha256=" + hex HMAC-SHA256(secret, body). The server
// verifies this constant-time, so a holder of the secret is the only party that
// can produce a request the server accepts.
func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
