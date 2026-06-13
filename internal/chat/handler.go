package chat

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

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

func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	msgs, err := h.Repo.Recent(r.Context(), h.CommunityID, RecentLimit)
	if err != nil {
		http.Error(w, "load chat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var oldest *time.Time
	if len(msgs) == RecentLimit {
		t := msgs[len(msgs)-1].CreatedAt
		oldest = &t
	}
	_ = webtempl.ChatPage(webtempl.ChatPageData{
		Viewer:     h.viewer(r),
		IsMod:      id.Membership.Role.AtLeast(auth.RoleMod),
		Messages:   toMsgViews(msgs),
		OldestSeen: oldest,
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
	msg, err := h.Svc.Send(r.Context(), SendInput{
		CommunityID:  h.CommunityID,
		AuthorID:     id.User.ID,
		BodyMarkdown: body,
	})
	if err != nil {
		h.Log.Error("send", "err", err)
		return
	}
	msg.AuthorName = id.Membership.DisplayName
	msg.AuthorAvatar = id.Membership.AvatarURL

	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	view := toMsgView(msg)

	// Append the message bubble immediately above the scroll anchor in this
	// sender's tab so they don't wait for the NATS round-trip.
	_ = sse.PatchElementTempl(
		webtempl.MessageView(view, isMod),
		datastar.WithSelector("#scroll-anchor"),
		datastar.WithModeBefore(),
	)
	// Re-patch the scroll anchor; its data-on-load fires again and scrolls to bottom.
	_ = sse.PatchElementTempl(webtempl.ScrollAnchor())
	// Clear the composer signal.
	_ = sse.PatchSignals([]byte(`{"body":""}`))

	// Fan-out to other tabs via NATS.
	if h.NATS != nil && h.NATS.IsConnected() {
		if buf, err := renderMessageFragment(r, msg, false); err == nil {
			_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), buf)
		}
	}
}

func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.FromContext(r.Context()); !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
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
		case m, ok := <-ch:
			if !ok {
				return
			}
			if err := sse.PatchElements(string(m.Data),
				datastar.WithSelector("#scroll-anchor"),
				datastar.WithModeBefore()); err != nil {
				return
			}
			if err := sse.PatchElementTempl(webtempl.ScrollAnchor()); err != nil {
				return
			}
		}
	}
}

func (h *Handler) GetOlder(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	beforeStr := r.URL.Query().Get("before")
	before, err := time.Parse(time.RFC3339Nano, beforeStr)
	if err != nil {
		http.Error(w, "bad before", http.StatusBadRequest)
		return
	}
	msgs, err := h.Repo.Before(r.Context(), h.CommunityID, before, RecentLimit)
	if err != nil {
		http.Error(w, "load: "+err.Error(), http.StatusInternalServerError)
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)
	sse := datastar.NewSSE(w, r)
	for _, m := range msgs {
		_ = sse.PatchElementTempl(
			webtempl.MessageView(toMsgView(m), isMod),
			datastar.WithSelector("#load-older"),
			datastar.WithModeAfter(),
		)
	}
	if len(msgs) == RecentLimit {
		t := msgs[len(msgs)-1].CreatedAt
		_ = sse.PatchElementTempl(webtempl.LoadOlderButton(t.Format(time.RFC3339Nano)))
	} else {
		_ = sse.PatchElementTempl(webtempl.NoOlder())
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
	w.WriteHeader(http.StatusOK)
}

func renderMessageFragment(r *http.Request, m Message, isMod bool) ([]byte, error) {
	var sb strings.Builder
	if err := webtempl.MessageView(toMsgView(m), isMod).Render(r.Context(), &sb); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
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
