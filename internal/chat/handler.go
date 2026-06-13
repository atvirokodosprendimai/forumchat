package chat

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"

	datastar "github.com/starfederation/datastar-go/datastar"
)

const RecentLimit = 50

type Handler struct {
	Svc           *Service
	Repo          *Repo
	NATS          *nats.Conn
	CommunityID   string
	CommunityName string
	Log           *slog.Logger
}

func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login?next=/chat", http.StatusSeeOther)
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
		CommunityName: h.CommunityName,
		DisplayName:   id.Membership.DisplayName,
		IsMod:         id.Membership.Role.AtLeast(auth.RoleMod),
		Messages:      toMsgViews(msgs),
		OldestSeen:    oldest,
	}).Render(r.Context(), w)
}

func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(r.PostFormValue("body"))
	if body == "" || len(body) > 4000 {
		http.Error(w, "invalid body length", http.StatusBadRequest)
		return
	}
	msg, err := h.Svc.Send(r.Context(), SendInput{
		CommunityID:  h.CommunityID,
		AuthorID:     id.User.ID,
		BodyMarkdown: body,
	})
	if err != nil {
		http.Error(w, "send: "+err.Error(), http.StatusInternalServerError)
		return
	}
	msg.AuthorName = id.Membership.DisplayName
	msg.AuthorAvatar = id.Membership.AvatarURL

	if h.NATS != nil && h.NATS.IsConnected() {
		buf, err := renderMessageFragment(r, msg, false)
		if err != nil {
			h.Log.Error("render fragment", "err", err)
		} else {
			_ = h.NATS.Publish(natsx.ChatSubject(h.CommunityID), buf)
		}
	}

	// Reply to sender with their own message appended via SSE-style response
	// so submitting via fetch updates immediately even before NATS round-trip.
	w.Header().Set("Content-Type", "text/event-stream")
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(
		webtempl.MessageView(toMsgView(msg), id.Membership.Role.AtLeast(auth.RoleMod)),
		datastar.WithSelector("#messages"),
		datastar.WithModeAppend(),
	)
}

func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	isMod := id.Membership.Role.AtLeast(auth.RoleMod)

	sse := render.NewSSE(w, r)

	if h.NATS == nil || !h.NATS.IsConnected() {
		// keep connection open without messages; will be closed on cancel
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

	_ = isMod // used by template; mod-specific delete buttons are pre-rendered at send time

	for {
		select {
		case <-r.Context().Done():
			return
		case m, ok := <-ch:
			if !ok {
				return
			}
			if err := sse.PatchElements(string(m.Data),
				datastar.WithSelector("#messages"),
				datastar.WithModeAppend()); err != nil {
				if errors.Is(err, http.ErrHandlerTimeout) {
					return
				}
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
	w.Header().Set("Content-Type", "text/event-stream")
	sse := render.NewSSE(w, r)
	// Patch each older message prepended (in chronological order so visually they appear above).
	for _, m := range msgs {
		_ = sse.PatchElementTempl(
			webtempl.MessageView(toMsgView(m), isMod),
			datastar.WithSelector("#messages"),
			datastar.WithModePrepend(),
		)
	}
	if len(msgs) == RecentLimit {
		t := msgs[len(msgs)-1].CreatedAt
		_ = sse.PatchElements(
			`<form method="get" action="/chat/older" id="load-older"><input type="hidden" name="before" value="`+t.Format(time.RFC3339Nano)+`"/><button type="submit">Load older</button></form>`,
			datastar.WithSelector("#load-older"),
			datastar.WithModeReplace(),
		)
	} else {
		_ = sse.PatchElements(`<div id="load-older" class="muted">— start of history —</div>`,
			datastar.WithSelector("#load-older"),
			datastar.WithModeReplace())
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

// renderMessageFragment renders a message bubble suitable for publishing
// over NATS for fan-out via the chat SSE stream.
func renderMessageFragment(r *http.Request, m Message, isMod bool) ([]byte, error) {
	var sb strings.Builder
	if err := webtempl.MessageView(toMsgView(m), isMod).Render(r.Context(), &sb); err != nil {
		return nil, err
	}
	return []byte(sb.String()), nil
}

func toMsgView(m Message) webtempl.MsgView {
	v := webtempl.MsgView{
		ID:           m.ID,
		AuthorName:   m.AuthorName,
		AuthorAvatar: m.AuthorAvatar,
		Kind:         webtempl.MsgKind(m.Kind),
		BodyHTML:     m.BodyHTML,
		CreatedAt:    m.CreatedAt,
		Deleted:      m.IsDeleted(),
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
