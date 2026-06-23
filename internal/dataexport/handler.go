package dataexport

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"database/sql"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler serves the owner-facing export UI: a live SSE status card, the request
// action, and the public token-gated download. The card + request routes are
// owner-gated in main.go; download is public (the high-entropy token is the
// capability, like /uploads).
type Handler struct {
	Svc *Service
	Log *slog.Logger
}

// GetStream live-streams the export status card for the current community. It
// re-patches #data-export whenever the underlying state changes (a new request,
// building → ready, expiry), so the owner sees progress without reloading.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	sse := render.NewSSE(w, r)
	last := ""
	push := func() string {
		e, found, err := h.Svc.Repo.Latest(r.Context(), c.ID)
		if err != nil {
			h.logf("dataexport: stream latest", "err", err)
			return last
		}
		key := stateKey(e, found)
		if key == last {
			return last
		}
		_ = sse.PatchElementTempl(webtempl.DataExportStatus(toView(absBase(r), c.Slug, e, found)))
		return key
	}
	last = push()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			last = push()
		}
	}
}

// PostRequest enqueues a new export, then patches the card to its fresh state.
// A request while one is in progress is a no-op (the card already shows it).
func (h *Handler) PostRequest(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var requestedBy string
	if id, ok := auth.FromContext(r.Context()); ok {
		requestedBy = id.User.ID
	}
	if _, err := h.Svc.Request(r.Context(), c.ID, requestedBy); err != nil && !errors.Is(err, ErrInProgress) {
		sse := render.NewSSE(w, r)
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("data-export-error", "Could not start export: "+err.Error()))
		return
	}
	e, found, err := h.Svc.Repo.Latest(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.DataExportStatus(toView(absBase(r), c.Slug, e, found)))
}

// GetLanding renders the PUBLIC, crawl-safe landing page for a shared export
// link. It NEVER streams the archive — a GET (which is what mail scanners and
// link unfurlers issue when they prefetch a link) only ever sees an HTML page.
// The actual bytes require the POST below, which prefetchers don't perform. An
// invalid/expired/unknown token renders the same generic "link no longer valid"
// page (no existence oracle), always 200.
func (h *Handler) GetLanding(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	token := r.URL.Query().Get("token")
	v := webtempl.ExportDownloadView{ID: id, Token: token}
	if e, ok := h.validDownload(r, id, token); ok {
		v.Valid = true
		v.SizeLabel = humanSize(e.SizeBytes)
		if e.ExpiresAt != nil {
			v.ExpiresAt = e.ExpiresAt.Local().Format("Jan 2, 2006 15:04")
		}
	}
	_ = webtempl.ExportDownloadPage(v).Render(r.Context(), w)
}

// PostDownload streams a ready export ZIP. Public + token-gated, but POST-only so
// that a link prefetch (GET) can never pull the data. The token arrives in the
// form body (not the URL), keeping it out of referrer/proxy logs. A
// missing/expired/mismatched export is a flat 404 — no existence oracle.
func (h *Handler) PostDownload(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// Plain HTML form POST (NOT a datastar signal handler), so form-parsing is
	// the correct way to read the token here.
	token := r.PostFormValue("token")
	e, ok := h.validDownload(r, id, token)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := fmt.Sprintf("export-%s.zip", e.CommunityID)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	http.ServeFile(w, r, h.Svc.ZipPath(e))
}

// validDownload loads an export and reports whether (id, token) is a live,
// downloadable capability. Shared by the landing page and the download POST.
func (h *Handler) validDownload(r *http.Request, id, token string) (Export, bool) {
	e, err := h.Svc.Repo.Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || err != nil {
		return Export{}, false
	}
	if token == "" || e.Token == "" || token != e.Token || !e.IsDownloadable(time.Now()) {
		return Export{}, false
	}
	return e, true
}

// absBase reconstructs this request's scheme://host so the owner card can show
// an absolute, copy-pasteable share link.
func absBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

// stateKey is the change-detection key for the stream: a new export id or a
// status change re-renders the card.
func stateKey(e Export, found bool) string {
	if !found {
		return "none"
	}
	return e.ID + ":" + e.Status
}

// toView maps the latest export onto the templ view model. base is this
// request's scheme://host, used to render an absolute, shareable landing link.
func toView(base, slug string, e Export, found bool) webtempl.DataExportView {
	v := webtempl.DataExportView{Slug: slug, State: "none"}
	if !found {
		return v
	}
	v.State = e.Status
	switch e.Status {
	case StatusReady:
		if !e.IsDownloadable(time.Now()) {
			v.State = StatusExpired // expired but not yet swept
			return v
		}
		v.ExportID = e.ID
		v.Token = e.Token
		v.LandingURL = fmt.Sprintf("%s/exports/%s?token=%s", base, e.ID, url.QueryEscape(e.Token))
		v.SizeLabel = humanSize(e.SizeBytes)
		if e.ExpiresAt != nil {
			v.ExpiresAt = e.ExpiresAt.Local().Format("Jan 2, 2006 15:04")
		}
	case StatusPending, StatusBuilding:
		v.RequestedAt = e.RequestedAt.Local().Format("15:04")
	case StatusFailed:
		v.Error = e.Error
	}
	return v
}

// humanSize formats a byte count as B / KB / MB / GB.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (h *Handler) logf(msg string, args ...any) {
	if h.Log != nil {
		h.Log.Warn(msg, args...)
	}
}
