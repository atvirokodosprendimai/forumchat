package webhooks

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	datastar "github.com/starfederation/datastar-go/datastar"

	natsgo "github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler serves the public inbound endpoint (/hooks/{token}) and the
// per-community admin CRUD page (/c/{slug}/admin/webhooks).
type Handler struct {
	Repo          *Repo
	Svc           *Service
	Chat          *chat.Service
	ChatRepo      *chat.Repo
	ChatBus       *chat.Bus
	ChatNewMsgBus *chat.Bus
	NATS          *natsgo.Conn
	BaseURL       string
	MaxBytes      int64
	Log           *slog.Logger
}

// ----- Inbound (public, token-authed) ---------------------------------------

// PostInbound is the public webhook receiver. The token in the URL is the only
// credential; a miss returns 404 (anti-enumeration). The provider adapter turns
// the body into a markdown bot message, which is posted into the webhook's
// target channel and fanned out exactly like the forum→chat bridge.
func (h *Handler) PostInbound(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	wh, err := h.Repo.InboundByToken(r.Context(), token)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	max := h.MaxBytes
	if max <= 0 {
		max = 1 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}

	if wh.Secret != "" && wh.Provider == "github" {
		if !verifyGitHubSignature(wh.Secret, body, r.Header.Get("X-Hub-Signature-256")) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
	}

	rendered, err := adapterFor(wh.Provider).Parse(r.Header, body)
	if err != nil {
		h.Log.Warn("webhooks: parse inbound", "provider", wh.Provider, "err", err)
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if rendered.Skip || strings.TrimSpace(rendered.Markdown) == "" {
		_ = h.Repo.Stamp(r.Context(), wh.ID, "skip")
		w.WriteHeader(http.StatusOK)
		return
	}

	if _, err := h.Chat.PostBot(r.Context(), wh.CommunityID, wh.ChannelID, wh.Name, wh.AvatarURL, rendered.Markdown); err != nil {
		h.Log.Error("webhooks: PostBot", "err", err)
		_ = h.Repo.Stamp(r.Context(), wh.ID, "error")
		http.Error(w, "post failed", http.StatusInternalServerError)
		return
	}
	h.fanout(wh.CommunityID, wh.ChannelID)
	_ = h.Repo.Stamp(r.Context(), wh.ID, "ok")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// fanout mirrors the forum→chat bridge: refresh open chat tabs (Bus + NATS) and
// ping the cross-page new-message listeners. channelID drives the per-channel
// fat-morph; an empty NATS payload would be a structural change instead.
func (h *Handler) fanout(communityID, channelID string) {
	if h.ChatBus != nil {
		h.ChatBus.Broadcast(channelID)
	}
	if h.ChatNewMsgBus != nil {
		h.ChatNewMsgBus.Broadcast("")
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.ChatSubject(communityID), []byte(channelID))
		_ = h.NATS.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
	}
}

// ----- Admin CRUD (RoleAdmin, per community) --------------------------------

type adminSignals struct {
	Direction string `json:"wh_direction"`
	Provider  string `json:"wh_provider"`
	Name      string `json:"wh_name"`
	AvatarURL string `json:"wh_avatar"`
	ChannelID string `json:"wh_channel"`
	Secret    string `json:"wh_secret"`
	TargetURL string `json:"wh_target"`
}

// GetAdmin renders the per-community webhooks admin page.
func (h *Handler) GetAdmin(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := h.pageData(r, cm)
	if err != nil {
		h.Log.Error("webhooks: page data", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	_ = webtempl.WebhooksPage(data).Render(r.Context(), w)
}

// PostCreate validates + persists a webhook, then re-renders the list. Inbound
// creation reveals the full URL once via the wh_new_url signal.
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
	wh, err := h.Svc.Create(r.Context(), CreateInput{
		CommunityID: cm.ID,
		Direction:   in.Direction,
		Provider:    in.Provider,
		Name:        in.Name,
		AvatarURL:   in.AvatarURL,
		ChannelID:   in.ChannelID,
		Secret:      in.Secret,
		TargetURL:   in.TargetURL,
		CreatedBy:   id.User.ID,
	})
	sse := render.NewSSE(w, r)
	if err != nil {
		_ = sse.PatchSignals([]byte(`{"wh_error":` + strconv.Quote(err.Error()) + `}`))
		return
	}
	h.renderList(sse, r, cm)
	reveal := ""
	if wh.Direction == DirIn {
		reveal = h.inboundURL(wh.Token)
	}
	_ = sse.PatchSignals([]byte(`{"wh_name":"","wh_avatar":"","wh_secret":"","wh_target":"","wh_channel":"","wh_error":"","wh_new_url":` + strconv.Quote(reveal) + `}`))
}

// PostToggle flips a webhook's enabled flag.
func (h *Handler) PostToggle(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	enabled := r.URL.Query().Get("enabled") == "1"
	if err := h.Repo.SetEnabled(r.Context(), cm.ID, r.URL.Query().Get("id"), enabled); err != nil {
		h.Log.Error("webhooks: toggle", "err", err)
	}
	sse := render.NewSSE(w, r)
	h.renderList(sse, r, cm)
}

// PostRotate mints a fresh inbound token and reveals the new URL once.
func (h *Handler) PostRotate(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	token, err := h.Svc.Rotate(r.Context(), cm.ID, r.URL.Query().Get("id"))
	sse := render.NewSSE(w, r)
	if err != nil {
		h.Log.Error("webhooks: rotate", "err", err)
		return
	}
	h.renderList(sse, r, cm)
	_ = sse.PatchSignals([]byte(`{"wh_new_url":` + strconv.Quote(h.inboundURL(token)) + `}`))
}

// PostDelete removes a webhook.
func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	if err := h.Repo.Delete(r.Context(), cm.ID, r.URL.Query().Get("id")); err != nil {
		h.Log.Error("webhooks: delete", "err", err)
	}
	sse := render.NewSSE(w, r)
	h.renderList(sse, r, cm)
}

// ----- helpers --------------------------------------------------------------

func (h *Handler) renderList(sse *datastar.ServerSentEventGenerator, r *http.Request, cm community.Community) {
	data, err := h.pageData(r, cm)
	if err != nil {
		h.Log.Error("webhooks: render list", "err", err)
		return
	}
	// WebhooksContent's root carries id="webhooks-root"; datastar morphs it
	// in place by id (§4.7 stable-id extract). No selector needed.
	_ = sse.PatchElementTempl(webtempl.WebhooksContent(data))
}

func (h *Handler) pageData(r *http.Request, cm community.Community) (webtempl.WebhooksPageData, error) {
	hooks, err := h.Repo.ListForCommunity(r.Context(), cm.ID)
	if err != nil {
		return webtempl.WebhooksPageData{}, err
	}
	channels, err := h.ChatRepo.ListChannels(r.Context(), cm.ID, false)
	if err != nil {
		return webtempl.WebhooksPageData{}, err
	}
	names := make(map[string]string, len(channels))
	opts := make([]webtempl.WebhookChannelOpt, 0, len(channels))
	for _, c := range channels {
		names[c.ID] = c.Name
		opts = append(opts, webtempl.WebhookChannelOpt{ID: c.ID, Name: c.Name})
	}

	var inbound, outbound []webtempl.WebhookRowView
	for _, wh := range hooks {
		row := webtempl.WebhookRowView{
			ID:          wh.ID,
			Provider:    wh.Provider,
			Name:        wh.Name,
			ChannelName: channelLabel(names, wh.ChannelID),
			Enabled:     wh.Enabled,
			LastStatus:  wh.LastStatus,
			LastAt:      lastAtLabel(wh.LastAt),
		}
		if wh.Direction == DirIn {
			row.URL = h.inboundURL(wh.Token)
			inbound = append(inbound, row)
		} else {
			row.TargetURL = wh.TargetURL
			outbound = append(outbound, row)
		}
	}

	return webtempl.WebhooksPageData{
		Viewer:   h.viewer(r, cm),
		Slug:     cm.Slug,
		Inbound:  inbound,
		Outbound: outbound,
		Channels: opts,
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

func (h *Handler) inboundURL(token string) string {
	return strings.TrimRight(h.BaseURL, "/") + "/hooks/" + token
}

func channelLabel(names map[string]string, id string) string {
	if id == "" {
		return "all channels"
	}
	if n, ok := names[id]; ok {
		return "#" + n
	}
	return "#?"
}

func lastAtLabel(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("15:04 Jan 2")
}
