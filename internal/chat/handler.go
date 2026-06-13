package chat

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

const RecentLimit = 100

type Handler struct {
	Svc           *Service
	Repo          *Repo
	NATS          *nats.Conn
	Bus           *Bus
	Uploads       *uploads.Store
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

const PasteImageMaxBytes = 1 << 20 // 1 MiB

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.CommunityName}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

func (h *Handler) loadRecent(ctx context.Context) ([]webtempl.MsgView, error) {
	msgs, err := h.Repo.Recent(ctx, h.CommunityID, RecentLimit)
	if err != nil {
		return nil, err
	}
	return toMsgViews(msgs), nil
}

// fatMorph emits the chat patches the UI expects:
//   1. #messages outer-morph → full latest-N list.
//   2. ExecuteScript → scroll #messages to its own bottom.
func fatMorph(sse *datastar.ServerSentEventGenerator, views []webtempl.MsgView, isMod bool, currentUserID string) error {
	if err := sse.PatchElementTempl(
		webtempl.MessagesContainer(views, isMod, currentUserID),
		datastar.WithModeOuter(),
	); err != nil {
		return err
	}
	return sse.ExecuteScript(
		`document.querySelector('#messages')?.scrollTo({top: 1e9, behavior: 'smooth'})`,
	)
}

// broadcast fans out a chat-changed signal locally (this process) AND over
// NATS (other processes). Either may be down; the other still works.
func (h *Handler) broadcast() {
	if h.Bus != nil {
		h.Bus.Broadcast()
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), []byte("changed"))
	}
}

func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	views, err := h.loadRecent(r.Context())
	if err != nil {
		http.Error(w, "load chat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = webtempl.ChatPage(webtempl.ChatPageData{
		Viewer:        h.viewer(r),
		IsMod:         id.Membership.Role.AtLeast(auth.RoleMod),
		CurrentUserID: id.User.ID,
		Messages:      views,
	}).Render(r.Context(), w)
}

type sendSignals struct {
	Body      string `json:"body"`
	ReplyToID string `json:"reply_to_id"`
	ImageData string `json:"image_data"`
}

func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	// 2 MB cap: 1 MB image after base64 (~1.33 MB on the wire) + text + JSON
	// framing. Defeats a runaway-paste turning into a memory grenade.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var in sendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(in.Body)
	sse := datastar.NewSSE(w, r)

	if in.ImageData != "" && h.Uploads != nil {
		u, err := h.Uploads.SaveDataURL(r.Context(), id.User.ID, h.CommunityID, in.ImageData, PasteImageMaxBytes)
		if err != nil {
			h.Log.Warn("paste image", "err", err)
		} else {
			url := h.Uploads.SignedURL(u.ID, id.User.ID, 24*time.Hour)
			imgMD := "![](" + url + ")"
			if body == "" {
				body = imgMD
			} else {
				body = imgMD + "\n\n" + body
			}
		}
	}

	if body == "" || len(body) > 4000 {
		return
	}
	var replyTo *string
	if rid := strings.TrimSpace(in.ReplyToID); rid != "" {
		replyTo = &rid
	}
	if _, err := h.Svc.Send(r.Context(), SendInput{
		CommunityID:  h.CommunityID,
		AuthorID:     id.User.ID,
		BodyMarkdown: body,
		ReplyToID:    replyTo,
	}); err != nil {
		h.Log.Error("send", "err", err)
		return
	}

	views, err := h.loadRecent(r.Context())
	if err == nil {
		_ = fatMorph(sse, views, id.Membership.Role.AtLeast(auth.RoleMod), id.User.ID)
	}
	// Clear composer signals.
	_ = sse.PatchSignals([]byte(`{"body":"","reply_to_id":"","image_data":""}`))

	h.broadcast()
}

func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	sse := datastar.NewSSE(w, r)

	// Initial sync: on every (re)connection — including when the browser
	// re-establishes SSE after tab sleep — push the latest 100 immediately.
	// Without this, a reconnecting client would see stale messages until the
	// next chat event fires.
	if views, err := h.loadRecent(r.Context()); err == nil {
		_ = fatMorph(sse, views, isMod, id.User.ID)
	}

	local, unsubscribe := h.Bus.Subscribe()
	defer unsubscribe()

	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 32)
		sub, err := h.NATS.ChanSubscribe(natsx.ChatSubject(h.CommunityID), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
			h.Log.Warn("nats subscribe", "err", err)
			natsCh = nil
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-local:
			// fall through to refresh
		case _, ok := <-natsCh:
			if !ok {
				natsCh = nil
				continue
			}
		}
		views, err := h.loadRecent(r.Context())
		if err != nil {
			continue
		}
		if err := fatMorph(sse, views, isMod, id.User.ID); err != nil {
			return
		}
	}
}

func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok || !id.Membership.Role.AtLeast(auth.RoleMod) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	msgID := r.URL.Query().Get("id")
	if msgID == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.SoftDelete(r.Context(), msgID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sse := datastar.NewSSE(w, r)
	views, err := h.loadRecent(r.Context())
	if err == nil {
		_ = fatMorph(sse, views, true, id.User.ID)
	}
	h.broadcast()
}

func toMsgView(m Message) webtempl.MsgView {
	v := webtempl.MsgView{
		ID:           m.ID,
		AuthorID:     valueOrEmpty(m.AuthorID),
		AuthorName:   m.AuthorName,
		AuthorAvatar: m.AuthorAvatar,
		Kind:         webtempl.MsgKind(m.Kind),
		BodyHTML:     m.BodyHTML,
		CreatedAt:    m.CreatedAt,
		Deleted:      m.IsDeleted(),
	}
	if m.ReplyTo != nil {
		v.ReplyTo = &webtempl.ReplySnippet{
			ID:         m.ReplyTo.ID,
			AuthorName: m.ReplyTo.AuthorName,
			Snippet:    m.ReplyTo.Snippet,
		}
	}
	return v
}

func toMsgViews(ms []Message) []webtempl.MsgView {
	out := make([]webtempl.MsgView, 0, len(ms))
	for _, m := range ms {
		out = append(out, toMsgView(m))
	}
	return out
}

func valueOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
