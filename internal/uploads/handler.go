package uploads

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
)

type Handler struct {
	Store       *Store
	CommunityID string
	Log         *slog.Logger
}

func (h *Handler) cid(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.ID
	}
	return h.cid(r)
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

	u, err := h.Store.Save(r.Context(), id.User.ID, h.cid(r), mime, file)
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
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	uploadID := chi.URLParam(r, "id")
	sig := r.URL.Query().Get("sig")
	expStr := r.URL.Query().Get("exp")
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		http.Error(w, "bad exp", http.StatusBadRequest)
		return
	}
	if err := h.Store.Verify(uploadID, id.User.ID, sig, exp); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	u, err := h.Store.Get(r.Context(), uploadID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if u.CommunityID != h.cid(r) {
		http.Error(w, "cross-community", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", u.MIME)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, h.Store.PathFor(u))
}
