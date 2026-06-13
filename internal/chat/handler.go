package chat

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

const RecentLimit = 100

type Handler struct {
	Svc           *Service
	Repo          *Repo
	NATS          *nats.Conn
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: h.CommunityName}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// loadRecent pulls the latest N messages and returns them as MsgView in
// newest-first order (the template renders them reversed).
func (h *Handler) loadRecent(ctx context.Context) ([]webtempl.MsgView, error) {
	msgs, err := h.Repo.Recent(ctx, h.CommunityID, RecentLimit)
	if err != nil {
		return nil, err
	}
	return toMsgViews(msgs), nil
}

// fatMorph emits the two SSE patches the chat UI expects:
//   1. #messages outer → full latest-N list (idiomorph diff handles existing DOM)
//   2. #scroll-anchor outer → fresh anchor whose data-init scrolls to bottom
//
// Sender's own tab and every other open tab on the channel see the same morph.
func fatMorph(sse *datastar.ServerSentEventGenerator, views []webtempl.MsgView, isMod bool) error {
	if err := sse.PatchElementTempl(
		webtempl.MessagesContainer(views, isMod),
		datastar.WithModeOuter(),
	); err != nil {
		return err
	}
	return sse.PatchElementTempl(
		webtempl.ScrollAnchor(),
		datastar.WithModeOuter(),
	)
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
		Viewer:   h.viewer(r),
		IsMod:    id.Membership.Role.AtLeast(auth.RoleMod),
		Messages: views,
	}).Render(r.Context(), w)
}

type sendSignals struct {
	Body string `json:"body"`
}

func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in sendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(in.Body)
	sse := datastar.NewSSE(w, r)
	if body == "" || len(body) > 4000 {
		return
	}
	if _, err := h.Svc.Send(r.Context(), SendInput{
		CommunityID:  h.CommunityID,
		AuthorID:     id.User.ID,
		BodyMarkdown: body,
	}); err != nil {
		h.Log.Error("send", "err", err)
		return
	}

	// 1. Fat-morph the latest 100 to the sender.
	views, err := h.loadRecent(r.Context())
	if err == nil {
		_ = fatMorph(sse, views, id.Membership.Role.AtLeast(auth.RoleMod))
	}
	// 2. Clear composer signal.
	_ = sse.PatchSignals([]byte(`{"body":""}`))

	// 3. Ping NATS so other open tabs refetch + fat-morph too.
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), []byte("changed"))
	}
}

func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	sse := datastar.NewSSE(w, r)

	if h.NATS == nil || !h.NATS.IsConnected() {
		<-r.Context().Done()
		return
	}
	ch := make(chan *nats.Msg, 32)
	sub, err := h.NATS.ChanSubscribe(natsx.ChatSubject(h.CommunityID), ch)
	if err != nil {
		h.Log.Error("nats subscribe", "err", err)
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-ch:
			if !ok {
				return
			}
			views, err := h.loadRecent(r.Context())
			if err != nil {
				continue
			}
			if err := fatMorph(sse, views, isMod); err != nil {
				return
			}
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
		_ = fatMorph(sse, views, true)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), []byte("changed"))
	}
}

func toMsgView(m Message) webtempl.MsgView {
	return webtempl.MsgView{
		ID:           m.ID,
		AuthorName:   m.AuthorName,
		AuthorAvatar: m.AuthorAvatar,
		Kind:         webtempl.MsgKind(m.Kind),
		BodyHTML:     m.BodyHTML,
		CreatedAt:    m.CreatedAt,
		Deleted:      m.IsDeleted(),
	}
}

func toMsgViews(ms []Message) []webtempl.MsgView {
	out := make([]webtempl.MsgView, 0, len(ms))
	for _, m := range ms {
		out = append(out, toMsgView(m))
	}
	return out
}
