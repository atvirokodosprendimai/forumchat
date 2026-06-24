package search

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler serves the community Search page and its SSE results endpoint.
type Handler struct {
	Svc           *Service
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

func (h *Handler) cid(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.ID
	}
	return h.CommunityID
}

func (h *Handler) cname(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.Name
	}
	return h.CommunityName
}

func (h *Handler) cslug(ctx context.Context) string {
	if c, ok := community.FromContext(ctx); ok {
		return c.Slug
	}
	return ""
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.cname(r.Context()), CommunitySlug: h.cslug(r.Context())}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetIndex renders the search page shell with an empty results region.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	_ = webtempl.SearchPage(webtempl.SearchPageData{Viewer: h.viewer(r)}).Render(r.Context(), w)
}

// GetResults runs the fused search for the `q` signal and morphs #search-results.
// Datastar drives it via a debounced @get, so signals arrive on the query string.
func (h *Handler) GetResults(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Q string `json:"q"`
	}
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	viewerID := ""
	if id, ok := auth.FromContext(r.Context()); ok {
		viewerID = id.User.ID
	}
	results, err := h.Svc.Search(r.Context(), h.cid(r.Context()), viewerID, h.cslug(r.Context()), in.Q, DefaultLimit)
	sse := render.NewSSE(w, r)
	if err != nil {
		h.Log.Error("search results", "err", err)
		_ = sse.PatchElementTempl(webtempl.SearchResults(webtempl.SearchResultsData{Query: in.Q, Err: true}))
		return
	}
	_ = sse.PatchElementTempl(webtempl.SearchResults(toResultsData(in.Q, results)))
}

func toResultsData(query string, rs []Result) webtempl.SearchResultsData {
	return webtempl.SearchResultsData{Query: query, Results: Views(rs)}
}

// Views maps fused results to view models, dropping any whose link failed to
// resolve (the underlying row was deleted between index and query). Shared by
// the search page handler and the chat /search slash command.
func Views(rs []Result) []webtempl.SearchResultView {
	out := make([]webtempl.SearchResultView, 0, len(rs))
	for _, r := range rs {
		if r.URL == "" {
			continue
		}
		out = append(out, webtempl.SearchResultView{
			Kind:       r.Kind,
			Title:      r.Title,
			Snippet:    r.Snippet,
			URL:        r.URL,
			When:       time.Unix(r.CreatedAt, 0).In(time.Local).Format("2006-01-02 15:04"),
			InFulltext: r.InFulltext,
			InSemantic: r.InSemantic,
		})
	}
	return out
}
