package worklog

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// RecentLimit caps how many past sessions the journal renders.
const RecentLimit = 200

type Handler struct {
	Repo *Repo
	Log  *slog.Logger
}

func (h *Handler) viewer(r *http.Request) webtempl.Viewer {
	v := webtempl.Viewer{}
	// /journal is top-level (no slug), but a community may still be in ctx
	// on some paths — pick it up when present so the sidebar can highlight.
	if c, ok := community.FromContext(r.Context()); ok {
		v.CommunityName = c.Name
		v.CommunitySlug = c.Slug
	}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// GetPage renders the personal journal page.
func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	panel, err := h.panel(r.Context(), id.User.ID, "")
	if err != nil {
		http.Error(w, "load journal: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = webtempl.JournalPage(webtempl.JournalPageData{
		Viewer: h.viewer(r),
		Panel:  panel,
	}).Render(r.Context(), w)
}

// PostStart begins the timer (idempotent) and morphs the panel.
func (h *Handler) PostStart(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	if _, err := h.Repo.Start(r.Context(), id.User.ID); err != nil {
		h.Log.Error("timer start", "err", err)
		return
	}
	h.patch(w, r, id.User.ID, "", false)
}

// PostStop ends the timer and morphs the panel into the "what did you do?"
// note prompt for the just-ended session (§4.9 server-driven step).
func (h *Handler) PostStop(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	sess, err := h.Repo.Stop(r.Context(), id.User.ID)
	if err != nil {
		// No active timer (e.g. stopped from another tab) — just resync.
		h.patch(w, r, id.User.ID, "", false)
		return
	}
	h.patch(w, r, id.User.ID, sess.ID, false)
}

// PostNote saves the journal note for a session, then morphs back to idle.
// Session id rides in ?id= (templated into the note form's @post URL).
func (h *Handler) PostNote(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	sid := strings.TrimSpace(r.URL.Query().Get("id"))
	var in struct {
		Note string `json:"worklog_note"`
	}
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	if sid != "" {
		if err := h.Repo.SetNote(r.Context(), id.User.ID, sid, strings.TrimSpace(in.Note)); err != nil {
			h.Log.Error("timer note", "err", err)
		}
	}
	h.patch(w, r, id.User.ID, "", true)
}

// patch re-renders #worklog (outer-morph). When the panel is in the running
// state its data-init restarts the JS elapsed clock on morph. clearNote wipes
// the note textarea signal.
func (h *Handler) patch(w http.ResponseWriter, r *http.Request, userID, noteSessionID string, clearNote bool) {
	panel, err := h.panel(r.Context(), userID, noteSessionID)
	if err != nil {
		h.Log.Error("worklog panel", "err", err)
		return
	}
	sse := render.NewSSE(w, r)
	if clearNote {
		_ = sse.PatchSignals([]byte(`{"worklog_note":""}`))
	}
	_ = sse.PatchElementTempl(webtempl.WorklogPanel(panel), datastar.WithModeOuter())
}

func (h *Handler) panel(ctx context.Context, userID, noteSessionID string) (webtempl.WorklogView, error) {
	active, hasActive, err := h.Repo.ActiveSession(ctx, userID)
	if err != nil {
		return webtempl.WorklogView{}, err
	}
	recent, err := h.Repo.ListRecent(ctx, userID, RecentLimit)
	if err != nil {
		return webtempl.WorklogView{}, err
	}

	state := "idle"
	var startUnix int64
	noteDur := 0
	switch {
	case hasActive:
		state = "running"
		startUnix = active.StartedAt.Unix()
	case noteSessionID != "":
		state = "note"
		for _, s := range recent {
			if s.ID == noteSessionID {
				noteDur = s.DurationMinutes()
				break
			}
		}
	}

	today := time.Now().Format("2006-01-02")
	todayMin := 0
	for _, s := range recent {
		if s.StartedAt.Format("2006-01-02") == today {
			todayMin += s.DurationMinutes()
		}
	}

	return webtempl.WorklogView{
		State:           state,
		ActiveStartUnix: startUnix,
		NoteSessionID:   noteSessionID,
		NoteDurationMin: noteDur,
		TodayMin:        todayMin,
		Days:            groupByDay(recent),
	}, nil
}

func groupByDay(recent []Session) []webtempl.WorklogDay {
	order := []string{}
	byDay := map[string]*webtempl.WorklogDay{}
	for _, s := range recent {
		if s.EndedAt == nil {
			continue
		}
		key := s.StartedAt.Format("2006-01-02")
		d := byDay[key]
		if d == nil {
			d = &webtempl.WorklogDay{Label: dayLabel(s.StartedAt)}
			byDay[key] = d
			order = append(order, key)
		}
		d.TotalMin += s.DurationMinutes()
		d.Sessions = append(d.Sessions, webtempl.WorklogSession{
			TimeRange:   s.StartedAt.Format("15:04") + "–" + s.EndedAt.Format("15:04"),
			DurationMin: s.DurationMinutes(),
			Note:        s.Note,
		})
	}
	out := make([]webtempl.WorklogDay, 0, len(order))
	for _, k := range order {
		out = append(out, *byDay[k])
	}
	return out
}

func dayLabel(t time.Time) string {
	now := time.Now()
	y, m, d := t.Date()
	if ny, nm, nd := now.Date(); y == ny && m == nm && d == nd {
		return "Today"
	}
	yest := now.AddDate(0, 0, -1)
	if yy, ym, yd := yest.Date(); y == yy && m == ym && d == yd {
		return "Yesterday"
	}
	return t.Format("Mon, 2 Jan 2006")
}
