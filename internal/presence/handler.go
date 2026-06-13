package presence

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

type Handler struct {
	Tracker     *Tracker
	CommunityID string
	Log         *slog.Logger
}

func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	sse := render.NewSSE(w, r)
	ch, cancel := h.Tracker.Watch(h.CommunityID)
	defer cancel()

	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	// Send initial state + heartbeat the user as present.
	h.Tracker.Touch(h.CommunityID, Member{
		UserID: id.User.ID, DisplayName: id.Membership.DisplayName, AvatarURL: id.Membership.AvatarURL,
	})
	h.push(sse)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			h.push(sse)
		case <-heartbeat.C:
			h.Tracker.Touch(h.CommunityID, Member{
				UserID: id.User.ID, DisplayName: id.Membership.DisplayName, AvatarURL: id.Membership.AvatarURL,
			})
			h.push(sse)
		}
	}
}

func (h *Handler) push(sse *datastar.ServerSentEventGenerator) {
	members := h.Tracker.Members(h.CommunityID)
	var sb strings.Builder
	sb.WriteString(`<aside id="presence" class="presence"><h3>Online · `)
	sb.WriteString(itoa(len(members)))
	sb.WriteString(`</h3><ul>`)
	for _, m := range members {
		sb.WriteString(`<li>`)
		sb.WriteString(escape(m.DisplayName))
		sb.WriteString(`</li>`)
	}
	sb.WriteString(`</ul></aside>`)
	_ = sse.PatchElements(sb.String(),
		datastar.WithSelector("#presence"),
		datastar.WithModeReplace())
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
