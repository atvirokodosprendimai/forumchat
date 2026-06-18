package presence

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// MemberLister supplies the full approved-member roster the sidebar
// renders (online + offline). Satisfied by *auth.Repo.ListMembers. Kept
// as a local interface so presence depends on a method, not the repo.
type MemberLister interface {
	ListMembers(ctx context.Context, communityID string) ([]auth.MemberRow, error)
}

type Handler struct {
	Tracker     *Tracker
	Members     MemberLister
	CommunityID string
	Log         *slog.Logger
}

func (h *Handler) cid(r *http.Request) string {
	if c, ok := community.FromContext(r.Context()); ok {
		return c.ID
	}
	return h.CommunityID
}

func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	sse := render.NewSSE(w, r)
	ch, cancel := h.Tracker.Watch(h.cid(r))
	defer cancel()

	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()

	// Send initial state + heartbeat the user as present.
	h.Tracker.Touch(h.cid(r), Member{
		UserID: id.User.ID, DisplayName: id.Membership.DisplayName, AvatarURL: id.Membership.AvatarURL,
	})
	cid := h.cid(r)
	h.push(r.Context(), sse, cid)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			h.push(r.Context(), sse, cid)
		case <-heartbeat.C:
			h.Tracker.Touch(cid, Member{
				UserID: id.User.ID, DisplayName: id.Membership.DisplayName, AvatarURL: id.Membership.AvatarURL,
			})
			h.push(r.Context(), sse, cid)
		}
	}
}

// push renders the full roster — every approved member, split into
// online (present in the Tracker) and offline groups — and morphs it
// into #presence-list. The roster carries each member's membership id +
// role so the right-click UserContextMenu can drive moderation actions.
func (h *Handler) push(ctx context.Context, sse *datastar.ServerSentEventGenerator, communityID string) {
	online := map[string]bool{}
	for _, m := range h.Tracker.Members(communityID) {
		online[m.UserID] = true
	}

	var rows []auth.MemberRow
	if h.Members != nil {
		var err error
		rows, err = h.Members.ListMembers(ctx, communityID)
		if err != nil && h.Log != nil {
			h.Log.Error("presence roster list", "err", err)
		}
	}

	now := time.Now()
	var on, off []webtempl.RosterMember
	for _, mr := range rows {
		rm := webtempl.RosterMember{
			UserID:       mr.UserID,
			MembershipID: mr.ID,
			DisplayName:  mr.DisplayName,
			AvatarURL:    mr.AvatarURL,
			Role:         string(mr.Role),
			Online:       online[mr.UserID],
			Banned:       mr.IsBanned(now),
		}
		if rm.Online {
			on = append(on, rm)
		} else {
			off = append(off, rm)
		}
	}
	_ = sse.PatchElementTempl(webtempl.RosterPanel(on, off))
}
