package pastes

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

type Handler struct {
	Svc      *Service
	Repo     *Repo
	ChatRepo *chat.Repo
	// PostToChat posts a paste's URL into a channel as the member and fans it
	// out (Bus + NATS + relay). Wired in main.go to avoid a chat import cycle —
	// same shape as the agent share-to-channel closure.
	PostToChat func(ctx context.Context, communityID, channelID, authorID, bodyMarkdown string) error
	// BaseURL is the public origin (e.g. https://chat.example.com) used to build
	// the ABSOLUTE paste link posted to chat, so relayed bots (Matrix etc.) get a
	// clickable URL rather than a host-relative /c/… path.
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

// GetPage renders a paste. The author of a draft sees the editor; everyone
// else — and the author once it's posted — sees the read-only rendered view.
func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p, err := h.Repo.ByID(r.Context(), chi.URLParam(r, "id"))
	if err != nil || p.CommunityID != h.cid(r.Context()) {
		http.NotFound(w, r)
		return
	}
	data := webtempl.PastePageData{
		Viewer: h.viewer(r),
		Slug:   h.cslug(r.Context()),
		Edit:   p.IsDraft() && p.AuthorID == id.User.ID,
		Paste:  toView(p),
	}
	_ = webtempl.PastePage(data).Render(r.Context(), w)
}

// PostNew creates a draft paste opened from the ?channel=<slug> channel and
// redirects (client-side) to its editor page.
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
	p, err := h.Svc.CreateDraft(r.Context(), h.cid(r.Context()), channelID, id.User.ID)
	if err != nil {
		h.Log.Error("paste new", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/pastes/" + p.ID)
}

type saveSignals struct {
	Title    string `json:"paste_title"`
	Language string `json:"paste_language"`
	Body     string `json:"paste_body"`
}

// PostSave persists a draft paste, posts its URL into the source channel as the
// member, and redirects back to that channel.
func (h *Handler) PostSave(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, (MaxBodyBytes + (64 << 10)))
	var in saveSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	pid := chi.URLParam(r, "id")
	cid := h.cid(r.Context())
	sse := render.NewSSE(w, r)

	p, err := h.Svc.Save(r.Context(), SaveInput{
		ID:          pid,
		CommunityID: cid,
		AuthorID:    id.User.ID,
		Title:       in.Title,
		Language:    in.Language,
		Body:        in.Body,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrEmpty):
			_ = sse.PatchElementTempl(webtempl.PasteError("Nothing to save — paste something first."))
		case errors.Is(err, ErrNotDraft), errors.Is(err, ErrForbidden):
			_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/pastes/" + pid)
		default:
			h.Log.Error("paste save", "err", err)
			_ = sse.PatchElementTempl(webtempl.PasteError("Couldn't save the paste. Try again."))
		}
		return
	}

	// Resolve the channel to post into + return to. Falls back to #general when
	// the source channel is gone (channel_id was SET NULL).
	channelID, channelSlug := h.resolveChannel(r.Context(), cid, p.ChannelID)
	if channelID != "" && h.PostToChat != nil {
		body := pasteMessage(h.BaseURL, h.cslug(r.Context()), p)
		if err := h.PostToChat(r.Context(), cid, channelID, id.User.ID, body); err != nil {
			h.Log.Error("paste post-to-chat", "err", err)
		}
	}
	target := "/c/" + h.cslug(r.Context()) + "/chat"
	if channelSlug != "" {
		target += "/" + channelSlug
	}
	_ = sse.Redirect(target)
}

// resolveChannel returns the (id, slug) to post into. Prefers the paste's
// source channel; falls back to the community's #general.
func (h *Handler) resolveChannel(ctx context.Context, communityID string, channelID *string) (string, string) {
	if channelID != nil {
		if ch, err := h.ChatRepo.ChannelByID(ctx, *channelID); err == nil {
			return ch.ID, ch.Slug
		}
	}
	if ch, err := h.ChatRepo.DefaultChannel(ctx, communityID); err == nil {
		return ch.ID, ch.Slug
	}
	return "", ""
}

// pasteMessage is the chat message body announcing a saved paste: a markdown
// link whose text is the paste title and whose href is the ABSOLUTE paste URL.
// Absolute (baseURL-prefixed) so relayed bots — Matrix etc. — get a clickable
// link instead of a host-relative /c/… path. Falls back to a relative URL when
// baseURL is unset. Brackets are stripped from the title so they can't break
// the markdown link.
func pasteMessage(baseURL, slug string, p Paste) string {
	url := strings.TrimRight(baseURL, "/") + "/c/" + slug + "/pastes/" + p.ID
	title := strings.NewReplacer("[", "", "]", "").Replace(p.Title)
	if strings.TrimSpace(title) == "" {
		title = "Paste"
	}
	return "📋 [" + title + "](" + url + ")"
}

func toView(p Paste) webtempl.PasteView {
	return webtempl.PasteView{
		ID:        p.ID,
		Title:     p.Title,
		Language:  p.Language,
		Body:      p.Body,
		BodyHTML:  p.BodyHTML,
		IsDraft:   p.IsDraft(),
		CreatedAt: p.CreatedAt,
	}
}
