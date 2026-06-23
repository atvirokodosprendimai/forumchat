package sendtoken

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

// rejectToast reuses the layout's existing pm-toast signal to surface a brief,
// non-fatal "couldn't send" message. The window is ~3 minutes, so a legitimate
// client (which holds a token refreshed by the SSE stream every 25s) effectively
// never sees this — only a stale tab or an automated client that didn't track
// the stream does, and a stale tab self-heals on its next stream tick.
const rejectToast = `{"_pm_toast_text":"Couldn't send — give the page a moment, then try again."}`

// Require returns middleware that rejects a member-write POST unless it carries
// a valid current/recent send token for the authenticated user.
//
// Apply it ONLY to JSON content-send routes: it buffers the request body to
// peek the token, so it must never wrap a GET/SSE stream or a multipart upload.
// A nil *Signer (or an unauthenticated request) passes through unchecked.
func (s *Signer) Require() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := auth.FromContext(r.Context())
			if s == nil || !ok {
				next.ServeHTTP(w, r)
				return
			}
			// Buffer the body (bounded) to read the token, then restore it so
			// the handler's datastar.ReadSignals sees the full payload.
			body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			var probe struct {
				SendToken string `json:"send_token"`
			}
			_ = json.Unmarshal(body, &probe) // missing/invalid → empty → rejected
			if !s.Valid(id.User.ID, probe.SendToken) {
				sse := datastar.NewSSE(w, r)
				_ = sse.PatchSignals([]byte(rejectToast))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
