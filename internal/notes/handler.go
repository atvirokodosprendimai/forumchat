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
	BaseURL string
	// LookupCommunity resolves a community's (name, slug) by id for the public
	// token reader, which carries no slug in its URL. Wired in main.go from the
	// community repo (a closure keeps notes free of a community.Repo field).
	LookupCommunity func(ctx context.Context, id string) (name, slug string, ok bool)
	CommunityID     string
	CommunityName   string
	Log             *slog.Logger
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
	if canEdit {
		data.Channels = h.channelOptions(r.Context(), n.CommunityID)
	}
	_ = webtempl.NotePage(data).Render(r.Context(), w)
}

// channelOptions lists the community's non-archived channels for the share
// dialog. Best-effort: an error yields no options (the dialog falls back to the
// default channel server-side).
func (h *Handler) channelOptions(ctx context.Context, communityID string) []webtempl.NoteChannelOption {
	chs, err := h.ChatRepo.ListChannels(ctx, communityID, false)
	if err != nil {
		h.Log.Error("notes channels", "err", err)
		return nil
	}
	out := make([]webtempl.NoteChannelOption, 0, len(chs))
	for _, c := range chs {
		out = append(out, webtempl.NoteChannelOption{ID: c.ID, Slug: c.Slug, Name: c.Name})
	}
	return out
}

// GetShared renders a note read-only from its capability-token link. Public and
// anon-readable (identity-optional): the token is the bearer capability. Any
// miss renders the generic "unavailable" page — no existence oracle. This is the
// only way to read a PRIVATE note without being an editor.
func (h *Handler) GetShared(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		h.renderSharedMiss(w, r)
		return
	}
	n, err := h.Repo.ByShareToken(r.Context(), token)
	if err != nil {
		h.renderSharedMiss(w, r)
		return
	}
	name, slug, ok := "", "", false
	if h.LookupCommunity != nil {
		name, slug, ok = h.LookupCommunity(r.Context(), n.CommunityID)
	}
	if !ok {
		h.renderSharedMiss(w, r)
		return
	}
	v := webtempl.Viewer{CommunityName: name, CommunitySlug: slug}
	if id, authed := auth.FromContext(r.Context()); authed {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	data := webtempl.NotePageData{
		Viewer: v,
		Slug:   slug,
		Note:   h.toView(r.Context(), n, false),
		Shared: true,
	}
	_ = webtempl.NotePage(data).Render(r.Context(), w)
}

func (h *Handler) renderSharedMiss(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	_ = webtempl.NoteUnavailable(webtempl.Viewer{}).Render(r.Context(), w)
}

type shareSignals struct {
	Channel string `json:"note_share_channel"`
}

// PostShare posts the note's link into a channel as the member (editors only).
// The link is built from the note's PERSISTED visibility/token, so a stale
// editor signal can't post the wrong URL.
func (h *Handler) PostShare(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in shareSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	n, err := h.Repo.ByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || n.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	sse := render.NewSSE(w, r)
	if !n.CanEdit(id) {
		_ = sse.PatchElementTempl(webtempl.NoteShareStatus("You can't share this note."))
		return
	}
	channelID, channelSlug := h.resolveChannel(r.Context(), n.CommunityID, in.Channel)
	if channelID == "" || h.PostToChat == nil {
		_ = sse.PatchElementTempl(webtempl.NoteShareStatus("No channel to post into."))
		return
	}
	body := noteMessage(h.shareLink(r.Context(), n), n.Title)
	if err := h.PostToChat(r.Context(), n.CommunityID, channelID, id.User.ID, body); err != nil {
		h.Log.Error("note share", "err", err)
		_ = sse.PatchElementTempl(webtempl.NoteShareStatus("Couldn't post the link. Try again."))
		return
	}
	_ = sse.PatchElementTempl(webtempl.NoteShareStatus("✓ Posted to #" + channelSlug))
}

// resolveChannel returns the (id, slug) to post into. Prefers the selected
// channel (validated to the community); falls back to #general.
func (h *Handler) resolveChannel(ctx context.Context, communityID, channelID string) (string, string) {
	if channelID != "" {
		if ch, err := h.ChatRepo.ChannelByID(ctx, channelID); err == nil && ch.CommunityID == communityID {
			return ch.ID, ch.Slug
		}
	}
	if ch, err := h.ChatRepo.DefaultChannel(ctx, communityID); err == nil {
		return ch.ID, ch.Slug
	}
	return "", ""
}

// shareLink is the absolute URL to post: the member route for a public note,
// the capability-token route for a private one. Mirrors web/templ shareURL but
// is the authoritative server-side build (uses persisted visibility/token).
func (h *Handler) shareLink(ctx context.Context, n Note) string {
	base := strings.TrimRight(h.BaseURL, "/")
	if n.IsPublic() {
		return base + "/c/" + h.cslug(ctx) + "/notes/" + n.ID
	}
	return base + "/n/" + n.ShareToken
}

// noteMessage is the chat body announcing a shared note: a markdown link whose
// text is the title and whose href is the absolute note URL. Brackets are
// stripped from the title so they can't break the link.
func noteMessage(url, title string) string {
	t := strings.NewReplacer("[", "", "]", "").Replace(strings.TrimSpace(title))
	if t == "" {
		t = "Note"
	}
	return "📝 [" + t + "](" + url + ")"
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
