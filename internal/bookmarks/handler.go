package bookmarks

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

type Handler struct {
	Repo          *Repo
	ChatRepo      *chat.Repo
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

func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	cats, _ := h.Repo.DistinctCategories(r.Context(), id.User.ID, h.cid(r.Context()))
	rows, _ := h.Repo.List(r.Context(), id.User.ID, h.cid(r.Context()), Filter{})
	_ = webtempl.BookmarksPage(webtempl.BookmarksPageData{
		Viewer:     h.viewer(r),
		Rows:       toViewRows(rows),
		Categories: cats,
	}).Render(r.Context(), w)
}

type filterSignals struct {
	BMTitle    string `json:"bm_title"`
	BMCategory string `json:"bm_category"`
	BMFrom     string `json:"bm_from"`
	BMTo       string `json:"bm_to"`
}

// GetList re-renders the #bookmarks-list region with current filter signals.
// Called on every filter input/change via datastar @get.
func (h *Handler) GetList(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in filterSignals
	_ = datastar.ReadSignals(r, &in)

	filter := Filter{
		Title:    strings.TrimSpace(in.BMTitle),
		Category: strings.TrimSpace(in.BMCategory),
	}
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(in.BMFrom)); err == nil {
		filter.From = t
	}
	if t, err := time.Parse("2006-01-02", strings.TrimSpace(in.BMTo)); err == nil {
		filter.To = t.Add(24 * time.Hour) // inclusive day
	}
	rows, err := h.Repo.List(r.Context(), id.User.ID, h.cid(r.Context()), filter)
	if err != nil {
		h.Log.Error("list bookmarks", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(
		webtempl.BookmarksList(toViewRows(rows), h.cslug(r.Context())),
		datastar.WithModeOuter(),
	)
}

type createSignals struct {
	Title    string `json:"bm_new_title"`
	Category string `json:"bm_new_category"`
	Note     string `json:"bm_new_note"`
}

func (h *Handler) PostCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	msgID := r.URL.Query().Get("id")
	if msgID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	msg, err := h.ChatRepo.ByID(r.Context(), msgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if msg.CommunityID != h.cid(r.Context()) {
		http.Error(w, "cross-community", http.StatusForbidden)
		return
	}
	var in createSignals
	_ = datastar.ReadSignals(r, &in)

	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = AutoTitleFromMarkdown(msg.BodyMarkdown)
	}
	b := Bookmark{
		UserID:        id.User.ID,
		CommunityID:   h.cid(r.Context()),
		ChatMessageID: msgID,
		Title:         title,
		Category:      strings.TrimSpace(in.Category),
		Note:          strings.TrimSpace(in.Note),
	}
	if _, err := h.Repo.Create(r.Context(), b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Reply: clear the per-message composer signal and close the inline form.
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"bm_open_msg":"","bm_new_title":"","bm_new_category":"","bm_new_note":""}`))
}

func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	bookmarkID := r.URL.Query().Get("id")
	if bookmarkID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.Delete(r.Context(), bookmarkID, id.User.ID); err != nil && !errors.Is(err, ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Re-render the list with the previously-applied filter signals (if any).
	var in filterSignals
	_ = datastar.ReadSignals(r, &in)
	filter := Filter{Title: in.BMTitle, Category: in.BMCategory}
	if t, err := time.Parse("2006-01-02", in.BMFrom); err == nil {
		filter.From = t
	}
	if t, err := time.Parse("2006-01-02", in.BMTo); err == nil {
		filter.To = t.Add(24 * time.Hour)
	}
	rows, _ := h.Repo.List(r.Context(), id.User.ID, h.cid(r.Context()), filter)
	sse := datastar.NewSSE(w, r)
	_ = sse.PatchElementTempl(
		webtempl.BookmarksList(toViewRows(rows), h.cslug(r.Context())),
		datastar.WithModeOuter(),
	)
}

func toViewRows(rows []Row) []webtempl.BookmarkRow {
	out := make([]webtempl.BookmarkRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, webtempl.BookmarkRow{
			ID:                r.ID,
			ChatMessageID:     r.ChatMessageID,
			Title:             r.Title,
			Category:          r.Category,
			Note:              r.Note,
			CreatedAt:         r.CreatedAt,
			MessageAuthorName: r.MessageAuthorName,
			MessageSnippet:    r.MessageSnippet,
			MessageCreatedAt:  r.MessageCreatedAt,
			MessageDeleted:    r.MessageDeleted,
		})
	}
	return out
}
