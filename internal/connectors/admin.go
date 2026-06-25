package connectors

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// adminSignals is the connector create/edit form payload. Channels and
// Capabilities are CSV strings because Datastar can't round-trip arrays through
// the signal bag (§6.7) — the checkboxes maintain a comma-separated list.
type adminSignals struct {
	ID           string `json:"con_id"`
	Name         string `json:"con_name"`
	AvatarURL    string `json:"con_avatar"`
	Channels     string `json:"con_channels"` // CSV of channel ids ("" = all)
	Caps         string `json:"con_caps"`     // CSV of capability tokens
	MentionsOnly bool   `json:"con_mentions"`
}

// GetAdmin renders the per-community connectors admin page.
func (h *Handler) GetAdmin(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := h.pageData(r, cm)
	if err != nil {
		h.Log.Error("connectors: page data", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	_ = webtempl.ConnectorsPage(data).Render(r.Context(), w)
}

// PostCreate provisions a connector and reveals its secret + URLs once.
func (h *Handler) PostCreate(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	id, ok2 := auth.FromContext(r.Context())
	if !ok || !ok2 {
		http.NotFound(w, r)
		return
	}
	var in adminSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	conn, err := h.Svc.Create(r.Context(), CreateInput{
		CommunityID:  cm.ID,
		Name:         in.Name,
		AvatarURL:    in.AvatarURL,
		ChannelIDs:   csv(in.Channels),
		Capabilities: csv(in.Caps),
		MentionsOnly: in.MentionsOnly,
		CreatedBy:    id.User.ID,
	})
	sse := render.NewSSE(w, r)
	if err != nil {
		_ = sse.PatchSignals([]byte(`{"con_error":` + strconv.Quote(err.Error()) + `}`))
		return
	}
	h.renderList(sse, r, cm)
	h.reveal(sse, conn)
}

// PostUpdate edits an existing connector (name / avatar / channels / caps /
// mentions). The secret is untouched (rotate is separate).
func (h *Handler) PostUpdate(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in adminSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if err := h.Svc.Update(r.Context(), UpdateInput{
		CommunityID:  cm.ID,
		ID:           in.ID,
		Name:         in.Name,
		AvatarURL:    in.AvatarURL,
		ChannelIDs:   csv(in.Channels),
		Capabilities: csv(in.Caps),
		MentionsOnly: in.MentionsOnly,
	}); err != nil {
		_ = sse.PatchSignals([]byte(`{"con_error":` + strconv.Quote(err.Error()) + `}`))
		return
	}
	h.renderList(sse, r, cm)
	// Reset the form back to "create" mode after a successful edit.
	_ = sse.PatchSignals([]byte(`{"con_id":"","con_name":"","con_avatar":"","con_channels":"","con_caps":"send","con_mentions":false,"con_error":""}`))
}

// PostToggle flips a connector's enabled flag.
func (h *Handler) PostToggle(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	enabled := r.URL.Query().Get("enabled") == "1"
	if err := h.Repo.SetEnabled(r.Context(), cm.ID, r.URL.Query().Get("id"), enabled); err != nil {
		h.Log.Error("connectors: toggle", "err", err)
	}
	sse := render.NewSSE(w, r)
	h.renderList(sse, r, cm)
}

// PostRotate mints a fresh secret and reveals the new secret + URLs once.
func (h *Handler) PostRotate(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	id := r.URL.Query().Get("id")
	if _, err := h.Svc.Rotate(r.Context(), cm.ID, id); err != nil {
		h.Log.Error("connectors: rotate", "err", err)
		return
	}
	conn, err := h.Repo.byIDInCommunity(r.Context(), cm.ID, id)
	sse := render.NewSSE(w, r)
	if err != nil {
		return
	}
	h.renderList(sse, r, cm)
	h.reveal(sse, conn)
}

// PostDelete removes a connector and its synthetic member.
func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.Svc.Delete(r.Context(), cm.ID, r.URL.Query().Get("id")); err != nil {
		h.Log.Error("connectors: delete", "err", err)
	}
	sse := render.NewSSE(w, r)
	h.renderList(sse, r, cm)
}

// ----- helpers --------------------------------------------------------------

// reveal patches the once-only secret + stream URL + send URL into the form
// callout. The secret is shown ONLY here (create / rotate) — it's never
// re-rendered into the list, like an API key.
func (h *Handler) reveal(sse *datastar.ServerSentEventGenerator, c Connector) {
	_ = sse.PatchSignals([]byte(`{` +
		`"con_new_secret":` + strconv.Quote(c.Secret) + `,` +
		`"con_new_stream":` + strconv.Quote(h.streamURL(c)) + `,` +
		`"con_new_send":` + strconv.Quote(h.sendURL(c)) + `,` +
		`"con_id":"","con_name":"","con_avatar":"","con_channels":"","con_caps":"send","con_mentions":false,"con_error":""}`))
}

func (h *Handler) renderList(sse *datastar.ServerSentEventGenerator, r *http.Request, cm community.Community) {
	data, err := h.pageData(r, cm)
	if err != nil {
		h.Log.Error("connectors: render list", "err", err)
		return
	}
	// ConnectorsContent's root carries id="connectors-root"; datastar morphs it
	// in place by id (§4.7 stable-id extract).
	_ = sse.PatchElementTempl(webtempl.ConnectorsContent(data))
}

func (h *Handler) pageData(r *http.Request, cm community.Community) (webtempl.ConnectorsPageData, error) {
	conns, err := h.Repo.ListForCommunity(r.Context(), cm.ID)
	if err != nil {
		return webtempl.ConnectorsPageData{}, err
	}
	channels, err := h.ChatRepo.ListChannels(r.Context(), cm.ID, false)
	if err != nil {
		return webtempl.ConnectorsPageData{}, err
	}
	names := make(map[string]string, len(channels))
	opts := make([]webtempl.ConnectorChannelOpt, 0, len(channels))
	for _, c := range channels {
		names[c.ID] = c.Name
		opts = append(opts, webtempl.ConnectorChannelOpt{ID: c.ID, Name: c.Name})
	}

	rows := make([]webtempl.ConnectorRowView, 0, len(conns))
	for _, c := range conns {
		chIDs, _ := h.Repo.Channels(r.Context(), c.ID)
		rows = append(rows, webtempl.ConnectorRowView{
			ID:           c.ID,
			Name:         c.Name,
			AvatarURL:    c.AvatarURL,
			ChannelIDs:   chIDs,
			ChannelLabel: channelsLabel(names, chIDs),
			Capabilities: c.Capabilities,
			MentionsOnly: c.MentionsOnly,
			Enabled:      c.Enabled,
			StreamURL:    h.streamURL(c),
			SendURL:      h.sendURL(c),
			LastStatus:   c.LastStatus,
			LastAt:       lastAtLabel(c.LastSeenAt),
		})
	}

	caps := make([]webtempl.ConnectorCapOpt, 0, len(KnownCapabilities))
	for _, k := range KnownCapabilities {
		caps = append(caps, webtempl.ConnectorCapOpt{Key: k, Label: capLabel(k), Desc: capDesc(k)})
	}

	return webtempl.ConnectorsPageData{
		Viewer:       h.viewer(r, cm),
		Slug:         cm.Slug,
		Connectors:   rows,
		Channels:     opts,
		Capabilities: caps,
	}, nil
}

func (h *Handler) viewer(r *http.Request, cm community.Community) webtempl.Viewer {
	v := webtempl.Viewer{CommunityName: cm.Name, CommunitySlug: cm.Slug}
	if id, ok := auth.FromContext(r.Context()); ok {
		v.IsAuthed = true
		v.DisplayName = id.Membership.DisplayName
		v.Role = string(id.Membership.Role)
	}
	return v
}

// streamURL builds the full signed, non-expiring stream URL for a connector.
func (h *Handler) streamURL(c Connector) string {
	return strings.TrimRight(h.BaseURL, "/") + "/bots/" + c.ID + "/stream?exp=0&sig=" + StreamSig(c.Secret, c.ID, 0)
}

// sendURL builds the (unsigned-in-URL) send endpoint; the worker signs the body.
func (h *Handler) sendURL(c Connector) string {
	return strings.TrimRight(h.BaseURL, "/") + "/bots/" + c.ID + "/send"
}

// csv splits a comma-separated signal value into trimmed, non-empty tokens.
func csv(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func channelsLabel(names map[string]string, ids []string) string {
	if len(ids) == 0 {
		return "all channels"
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if n, ok := names[id]; ok {
			parts = append(parts, "#"+n)
		}
	}
	return strings.Join(parts, ", ")
}

func capLabel(k string) string {
	switch k {
	case CapSend:
		return "Send messages"
	case CapDelete:
		return "Delete messages"
	case CapBan:
		return "Ban members"
	case CapRename:
		return "Rename channels"
	default:
		return k
	}
}

func capDesc(k string) string {
	switch k {
	case CapSend:
		return "Post messages into its channels (the base ability)."
	case CapDelete:
		return "Soft-delete any message (hidden from everyone)."
	case CapBan:
		return "Ban a member by id (admins/owners are protected)."
	case CapRename:
		return "Rename a channel (#general is protected)."
	default:
		return ""
	}
}

func lastAtLabel(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("15:04 Jan 2")
}
