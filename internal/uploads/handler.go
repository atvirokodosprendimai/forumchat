package uploads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
)

type Handler struct {
	Store       *Store
	CommunityID string
	Log         *slog.Logger
	// Sessions is optional; when set, GetFile resolves project share-
	// link guest sessions in addition to auth users so guests can view
	// images uploaded inside their project.
	Sessions *scs.SessionManager
	// MemberOf reports whether userID belongs to communityID. Injected from
	// main.go (auth.Repo.MembershipFor + super-admin bypass). When set,
	// GetFile gates an authenticated viewer to the upload's community so a
	// logged-in user can't read another tenant's media by guessing the id.
	// Optional: nil keeps the legacy permissive behaviour (tests).
	MemberOf func(ctx context.Context, userID, communityID string) bool
	// NoSessionFetchOK rate-limits the session-LESS shared-signed fetch path per
	// client (FIX1 M16): a leaked shared URL is a reusable bearer until it
	// expires (24h) with no revocation, so cap how fast one client can pull
	// through that path. Returns false when the caller is over the limit. nil =
	// no limit (the session path is unaffected either way).
	NoSessionFetchOK func(clientIP string) bool
}

// Project guest session keys — kept in sync with internal/projects/guest.go.
const (
	sessKeyProjectGuestID = "project_guest_id"
)

func (h *Handler) cid(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.ID
	}
	return h.CommunityID
}

const signedTTL = 24 * time.Hour

// PostUpload accepts a multipart file under field "file" and returns JSON-ish text
// with the markdown-ready image link signed for this user.
func (h *Handler) PostUpload(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseMultipartForm(h.Store.MaxSize + 1024); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	sniff := make([]byte, 512)
	n, _ := file.Read(sniff)
	sniff = sniff[:n]
	if _, err := file.Seek(0, 0); err != nil {
		http.Error(w, "seek: "+err.Error(), http.StatusInternalServerError)
		return
	}
	mime := MIMEFromHeader(hdr.Header.Get("Content-Type"), sniff)

	u, err := h.Store.Save(r.Context(), id.User.ID, h.cid(r), mime, hdr.Filename, file)
	if err != nil {
		switch {
		case errors.Is(err, ErrBadMIME):
			http.Error(w, "unsupported file type", http.StatusUnsupportedMediaType)
		case errors.Is(err, ErrTooLarge):
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		default:
			h.Log.Error("upload save", "err", err)
			http.Error(w, "upload failed", http.StatusInternalServerError)
		}
		return
	}
	url := h.Store.SignedURL(u.ID, id.User.ID, signedTTL)
	markdown := fmt.Sprintf("![](%s)", url)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, markdown)
}

func (h *Handler) GetFile(w http.ResponseWriter, r *http.Request) {
	uploadID := chi.URLParam(r, "id")
	viewerID := h.viewerID(r)
	if viewerID == "" {
		// No session. Admit ONLY when the request carries a valid, unexpired
		// shared signature — this lets a trusted external consumer (e.g. an
		// outbound-webhook receiver) fetch a shared-signed URL we minted,
		// without a forumchat session. Anything else stays rejected.
		if !hasValidSharedSig(h.Store, r, uploadID) {
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		// Throttle the no-session bearer-URL path per client (FIX1 M16) so a
		// leaked shared URL can't be hammered.
		if h.NoSessionFetchOK != nil && !h.NoSessionFetchOK(clientIP(r)) {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
	} else if sig := r.URL.Query().Get("sig"); sig != "" {
		// Authed viewer: HMAC verification is best-effort defense-in-depth
		// (the session gate above is the real access control). Stale/legacy
		// signatures still serve to any authenticated viewer.
		if expStr := r.URL.Query().Get("exp"); expStr != "" {
			if exp, err := strconv.ParseInt(expStr, 10, 64); err == nil {
				_ = h.Store.Verify(uploadID, viewerID, sig, exp)
			}
		}
	}
	u, err := h.Store.Get(r.Context(), uploadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	// Tenant boundary: an authenticated viewer must belong to the upload's
	// community (cross-tenant god-mode bypasses, self-host only — in SaaS the
	// operator gets no tenant media). The HMAC signature is only advisory on
	// the authed path (its Verify result is discarded above for stale/
	// legacy-URL compatibility), so without this any logged-in user could
	// read another community's media by guessing the id. Guests are scoped by
	// their project-share session; the no-session path required a valid
	// shared signature above. MemberOf nil keeps the legacy behaviour (tests).
	if id, ok := auth.FromContext(r.Context()); ok && !id.GodMode() {
		// Fail closed (FIX1 M28): an authenticated, non-god viewer must be a
		// member of the upload's community. The owner may always read their own
		// file. When MemberOf isn't wired we can't verify membership, so we deny
		// everyone but the owner rather than fall back to the old permissive
		// "any authed user reads any community's media" behaviour.
		if id.User.ID != u.OwnerID {
			if h.MemberOf == nil || !h.MemberOf(r.Context(), id.User.ID, u.CommunityID) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
		}
	}
	w.Header().Set("Content-Type", u.MIME)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// Stop the browser from MIME-sniffing the body into something executable
	// (FIX1 H1). Combined with the upload-time denylist (C2/C3) this means even
	// a pre-fix text/html row already on disk can't run JS from our origin.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Inline ONLY an explicit allowlist of known-safe media types; everything
	// else is forced to a download. A family check (image/*) is unsafe because
	// image/svg+xml is in the image family but carries inline <script> — a legacy
	// row stored before the denylist would still XSS if inlined (Codex caught
	// this). The list mirrors the safe-extension map (FIX1 C2/H2).
	disp := "attachment"
	switch baseMIME(u.MIME) {
	case "image/jpeg", "image/png", "image/gif", "image/webp",
		"video/mp4", "video/webm", "video/quicktime",
		"audio/mpeg", "audio/mp4", "audio/wav", "audio/ogg":
		disp = "inline"
	}
	w.Header().Set("Content-Disposition", contentDisposition(disp, u.Filename))
	if err := h.Store.Serve(w, r, u); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// contentDisposition builds a safe Content-Disposition header value. The stored
// filename is user-supplied; sanitiseFilename strips control bytes and path
// separators but NOT the double-quote, so the legacy quoted form has `\` and `"`
// escaped to stop a crafted filename from closing the quoted-string and
// injecting disposition parameters (FIX1 L5). A filename* (RFC 5987) variant
// carries the UTF-8 name for modern browsers. Empty filename → bare type.
func contentDisposition(dispType, filename string) string {
	if filename == "" {
		return dispType
	}
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(filename)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, dispType, quoted, url.PathEscape(filename))
}

// clientIP extracts the request's source IP (host part of RemoteAddr), used to
// key the no-session fetch rate limit. Falls back to the raw RemoteAddr when it
// has no port.
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// hasValidSharedSig reports whether the request carries a valid, unexpired
// shared signature for uploadID. Used to admit session-less fetches of
// shared-signed URLs (Verify rejects expired or non-matching signatures).
func hasValidSharedSig(store *Store, r *http.Request, uploadID string) bool {
	sig := r.URL.Query().Get("sig")
	expStr := r.URL.Query().Get("exp")
	if sig == "" || expStr == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	return store.Verify(uploadID, "", sig, exp) == nil
}

// viewerID returns the identity to verify the signed URL against. Auth
// users win; otherwise we look for a project-share guest session and
// build "guest:<gid>" — matching the synthetic viewer-id the projects
// package signs URLs with.
func (h *Handler) viewerID(r *http.Request) string {
	if id, ok := auth.FromContext(r.Context()); ok {
		return id.User.ID
	}
	if h.Sessions == nil {
		return ""
	}
	gid := h.Sessions.GetString(r.Context(), sessKeyProjectGuestID)
	if gid != "" {
		return "guest:" + gid
	}
	return ""
}
