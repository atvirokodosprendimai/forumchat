package uploads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	// community (super-admin bypasses). The HMAC signature is only advisory
	// on the authed path (its Verify result is discarded above for stale/
	// legacy-URL compatibility), so without this any logged-in user could
	// read another community's media by guessing the id. Guests are scoped by
	// their project-share session; the no-session path required a valid
	// shared signature above. MemberOf nil keeps the legacy behaviour (tests).
	if id, ok := auth.FromContext(r.Context()); ok && !id.IsSuperAdmin && h.MemberOf != nil {
		if !h.MemberOf(r.Context(), id.User.ID, u.CommunityID) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", u.MIME)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// Preserve the user-supplied filename for inline-rendered kinds AND
	// download-chip kinds. The browser inlines image/video/audio/pdf
	// regardless of disposition — for application/* and unknown kinds
	// the browser triggers a download with the right name instead of
	// "<sha>.bin".
	if u.Filename != "" {
		w.Header().Set("Content-Disposition", `inline; filename="`+u.Filename+`"`)
	}
	if err := h.Store.Serve(w, r, u); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
	}
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
