package devdocs

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/docs"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler serves the public developer documentation site at /dev/docs. It has
// no datastore and no auth: every page is rendered from the embedded docs
// package, the content is public, and a logger is the only dependency (used
// for the rare render failure).
type Handler struct {
	Log *slog.Logger
}

// New returns a docs Handler. log may be nil — render errors then fall back to
// a plain 500 with no log line.
func New(log *slog.Logger) *Handler {
	return &Handler{Log: log}
}

// Mount registers the two public doc routes on r. Both are plain GETs: the docs
// carry no secrets and are useful in self-host and SaaS alike, so they are
// mounted unconditionally outside the auth group.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/dev/docs", h.GetIndex)
	r.Get("/dev/docs/{slug}", h.GetDoc)
}

// GetIndex renders the docs landing: a hero plus one card per published doc.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := webtempl.DevDocsIndexData{Docs: navLinks("")}
	if err := webtempl.DevDocsIndex(data).Render(r.Context(), w); err != nil {
		h.fail(w, "render docs index", err)
	}
}

// GetDoc renders a single doc page: sidebar nav + on-this-page TOC + prose. An
// unknown slug is a 404 — the slug is matched against the trusted Manifest, so
// there is no way to read an arbitrary embedded file by path.
func (h *Handler) GetDoc(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	doc, src, ok := docs.Get(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}

	body, err := Render(src)
	if err != nil {
		h.fail(w, "render doc "+slug, err)
		return
	}

	data := webtempl.DevDocPageData{
		Title:    doc.Title,
		Summary:  doc.Summary,
		BodyHTML: body,
		TOC:      toTOC(TableOfContents(body)),
		Nav:      navLinks(slug),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := webtempl.DevDocPage(data).Render(r.Context(), w); err != nil {
		h.fail(w, "render doc page "+slug, err)
	}
}

// navLinks builds the cross-doc navigation list, flagging the active slug so the
// sidebar and index can highlight the current page. Used by both the index
// (active == "") and each doc page.
func navLinks(active string) []webtempl.DevDocLink {
	list := docs.List()
	out := make([]webtempl.DevDocLink, 0, len(list))
	for _, d := range list {
		out = append(out, webtempl.DevDocLink{
			Slug:    d.Slug,
			Title:   d.Title,
			Summary: d.Summary,
			Icon:    d.Icon,
			Active:  d.Slug == active,
		})
	}
	return out
}

// toTOC adapts the renderer's TOC entries to the templ view model (web/templ is
// a leaf package and cannot import this one — §4.13).
func toTOC(entries []TOCEntry) []webtempl.DevDocTOCItem {
	out := make([]webtempl.DevDocTOCItem, 0, len(entries))
	for _, e := range entries {
		out = append(out, webtempl.DevDocTOCItem{ID: e.ID, Title: e.Title, Sub: e.Sub})
	}
	return out
}

// fail logs (when a logger is present) and returns a generic 500. Render errors
// here mean a build/authoring bug in the embedded docs, not user input.
func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	if h.Log != nil {
		h.Log.Error("devdocs: "+what, "err", err)
	}
	http.Error(w, "documentation temporarily unavailable", http.StatusInternalServerError)
}
