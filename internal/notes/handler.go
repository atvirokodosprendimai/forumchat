package notes

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// listLimit caps each index column.
const listLimit = 200

type Handler struct {
	Svc      *Service
	Repo     *Repo
	ChatRepo *chat.Repo
	// PostToChat drops a note's URL into a channel as the member and fans it out
	// (Bus + NATS + relay). Wired in main.go to avoid a chat import cycle — same
	// shape as the pastes share closure.
	PostToChat func(ctx context.Context, communityID, channelID, authorID, bodyMarkdown string) error
	// BaseURL is the public origin used to build ABSOLUTE share links (so a
	// relayed bot gets a clickable URL, and the private token link is complete).
	BaseURL       string
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

// GetIndex lists the community's public notes plus the viewer's own notes.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	cid := h.cid(r.Context())
	pub, err := h.Repo.ListPublic(r.Context(), cid, listLimit)
	if err != nil {
		h.Log.Error("notes index public", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	mine, err := h.Repo.ListByAuthor(r.Context(), cid, id.User.ID, listLimit)
	if err != nil {
		h.Log.Error("notes index mine", "err", err)
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	data := webtempl.NotesPageData{
		Viewer: h.viewer(r),
		Slug:   h.cslug(r.Context()),
		Public: toListItems(pub),
		Mine:   toListItems(mine),
	}
	_ = webtempl.NotesIndex(data).Render(r.Context(), w)
}

// PostNew creates an empty private note and redirects (client-side) to its
// editor. ?channel=<slug> remembers the channel it was opened from.
func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	channelID := ""
	if slug := r.URL.Query().Get("channel"); slug != "" {
		if ch, err := h.ChatRepo.ChannelBySlug(r.Context(), h.cid(r.Context()), slug); err == nil {
			channelID = ch.ID
		}
	}
	n, err := h.Svc.CreateDraft(r.Context(), h.cid(r.Context()), channelID, id.User.ID)
	if err != nil {
		h.Log.Error("note new", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/notes/" + n.ID)
}

// GetPage renders a note. Editors (author/mod) get the editor + reader; other
// members get the reader of a public note. A private note is only reachable here
// by an editor — everyone else uses the token link (GetShared).
func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	n, err := h.Repo.ByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || n.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	canEdit := n.CanEdit(id)
	if !n.IsPublic() && !canEdit {
		http.NotFound(w, r)
		return
	}
	data := webtempl.NotePageData{
		Viewer:     h.viewer(r),
		Slug:       h.cslug(r.Context()),
		BaseURL:    h.BaseURL,
		Note:       h.toView(r.Context(), n, canEdit),
		Edit:       canEdit,
		CanComment: true,
	}
	_ = webtempl.NotePage(data).Render(r.Context(), w)
}

type saveSignals struct {
	Title      string `json:"note_title"`
	Body       string `json:"note_body"`
	Visibility string `json:"note_visibility"`
}

// PostSave persists an edit and re-renders the reader zone in place.
func (h *Handler) PostSave(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes+(64<<10))
	var in saveSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	n, err := h.Svc.Save(r.Context(), id, SaveInput{
		ID:         chi.URLParam(r, "id"),
		Title:      in.Title,
		Body:       in.Body,
		Visibility: in.Visibility,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrEmpty):
			_ = sse.PatchElementTempl(webtempl.NoteError("Nothing to save — write something first."))
		case errors.Is(err, ErrForbidden):
			_ = sse.PatchElementTempl(webtempl.NoteError("You can't edit this note."))
		default:
			h.Log.Error("note save", "err", err)
			_ = sse.PatchElementTempl(webtempl.NoteError("Couldn't save. Try again."))
		}
		return
	}
	_ = sse.PatchElementTempl(webtempl.NoteError(""))
	_ = sse.PatchElementTempl(webtempl.NoteReader(h.reader(r.Context(), n, true, true)))
}

type previewSignals struct {
	Body string `json:"note_body"`
}

// PostPreview renders the markdown body to HTML for the live preview pane. It is
// side-effect free (does not persist).
func (h *Handler) PostPreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes+(64<<10))
	var in previewSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	html, err := render.RenderMarkdown(strings.TrimSpace(in.Body))
	if err != nil {
		html = ""
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.NotePreview(html))
}

// PostDelete removes a note (editors only) and redirects to the index.
func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	n, err := h.Repo.ByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || n.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	sse := render.NewSSE(w, r)
	if !n.CanEdit(id) {
		_ = sse.PatchElementTempl(webtempl.NoteError("You can't delete this note."))
		return
	}
	if err := h.Repo.Delete(r.Context(), n.ID); err != nil {
		h.Log.Error("note delete", "err", err)
		_ = sse.PatchElementTempl(webtempl.NoteError("Couldn't delete. Try again."))
		return
	}
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/notes")
}

// reader builds the reader-zone component for a note.
func (h *Handler) reader(ctx context.Context, n Note, edit, canComment bool) webtempl.NotePageData {
	return webtempl.NotePageData{
		Slug:       h.cslug(ctx),
		BaseURL:    h.BaseURL,
		Note:       h.toView(ctx, n, edit),
		Edit:       edit,
		CanComment: canComment,
	}
}

func (h *Handler) toView(ctx context.Context, n Note, canEdit bool) webtempl.NoteView {
	return webtempl.NoteView{
		ID:         n.ID,
		Title:      n.Title,
		Body:       n.Body,
		BodyHTML:   n.BodyHTML,
		IsPublic:   n.IsPublic(),
		ShareToken: n.ShareToken,
		CanEdit:    canEdit,
		CreatedAt:  n.CreatedAt,
		UpdatedAt:  n.UpdatedAt,
	}
}

func toListItems(ns []Note) []webtempl.NoteListItem {
	out := make([]webtempl.NoteListItem, 0, len(ns))
	for _, n := range ns {
		out = append(out, webtempl.NoteListItem{
			ID:        n.ID,
			Title:     n.Title,
			IsPublic:  n.IsPublic(),
			Snippet:   snippet(n.Body, 110),
			UpdatedAt: n.UpdatedAt,
		})
	}
	return out
}

// snippet collapses a markdown body to a short single-line teaser, stripping
// leading markdown markers (#, -, *, >, backticks) so the list reads as prose.
func snippet(body string, n int) string {
	s := strings.Join(strings.Fields(body), " ")
	s = strings.TrimLeft(s, "#-*>` \t")
	if len(s) > n {
		s = strings.TrimSpace(s[:n]) + "…"
	}
	return s
}
