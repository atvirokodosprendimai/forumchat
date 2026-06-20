package agent

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"
)

// Handler is the HTTP boundary for the Agent feature.
type Handler struct {
	Repo   *Repo
	Svc    *Service
	Runner *Runner
	Bus    *Bus
	NATS   *nats.Conn
	Log    *slog.Logger

	CommunityID   string
	CommunityName string

	// ShareToChannel posts an assistant answer into a community chat channel
	// as the requesting member. Wired in main.go to chat's send+broadcast so
	// this package needn't import chat. Nil disables the share affordance.
	ShareToChannel func(ctx context.Context, communityID, channelSlug, authorID, bodyMD string) (channelName string, err error)
	// ListChannels lists the community's chat channels for the share dropdown.
	ListChannels func(ctx context.Context, communityID string) []webtempl.ChannelView
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

func threadID(r *http.Request) string { return chi.URLParam(r, "thread") }

// resolveReadable loads the routed thread and confirms the viewer may read it:
// shared threads are open to every member; private threads only to their
// creator. Writes a 404 and returns ok=false otherwise (404 not 403 so a
// private thread's existence isn't revealed).
func (h *Handler) resolveReadable(w http.ResponseWriter, r *http.Request) (Thread, auth.Identity, bool) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return Thread{}, auth.Identity{}, false
	}
	t, err := h.Repo.ThreadByID(r.Context(), threadID(r))
	if err != nil || t.CommunityID != h.cid(r.Context()) || !canRead(t, id.User.ID) {
		http.NotFound(w, r)
		return Thread{}, auth.Identity{}, false
	}
	return t, id, true
}

func canRead(t Thread, userID string) bool {
	return t.Visibility == VisibilityShared || t.UserID == userID
}

func canDelete(t Thread, userID string, isAdmin bool) bool {
	return t.UserID == userID || (isAdmin && t.Visibility == VisibilityShared)
}

// --- pages ----------------------------------------------------------------

// GetIndex (/agent) redirects to the newest visible thread, or renders the
// empty state when the member has none yet.
func (h *Handler) GetIndex(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	threads, _ := h.Repo.ListThreads(r.Context(), h.cid(r.Context()), id.User.ID)
	if len(threads) > 0 {
		http.Redirect(w, r, "/c/"+h.cslug(r.Context())+"/agent/"+threads[0].ID, http.StatusSeeOther)
		return
	}
	cfg, _ := h.Repo.GetConfig(r.Context(), h.cid(r.Context()))
	d := h.pageData(r, Thread{}, false, nil, threads, cfg.Enabled)
	_ = webtempl.AgentPage(d).Render(r.Context(), w)
}

// GetPage (/agent/{thread}) renders the two-pane page for one thread.
func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	t, id, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	threads, _ := h.Repo.ListThreads(r.Context(), h.cid(r.Context()), id.User.ID)
	msgs, _ := h.Repo.Messages(r.Context(), t.ID)
	cfg, _ := h.Repo.GetConfig(r.Context(), h.cid(r.Context()))
	d := h.pageData(r, t, true, msgs, threads, cfg.Enabled)
	_ = webtempl.AgentPage(d).Render(r.Context(), w)
}

func (h *Handler) pageData(r *http.Request, t Thread, hasActive bool, msgs []Message, threads []Thread, configured bool) webtempl.AgentPageData {
	slug := h.cslug(r.Context())
	canShare := h.ShareToChannel != nil
	d := webtempl.AgentPageData{
		Viewer:     h.viewer(r),
		Slug:       slug,
		HasActive:  hasActive,
		Configured: configured,
		CanShare:   canShare,
	}
	if hasActive {
		d.Active = webtempl.AgentThreadView{ID: t.ID, Title: t.Title, Visibility: t.Visibility}
		d.Generating = h.threadGenerating(t.ID, msgs)
		for _, m := range msgs {
			d.Messages = append(d.Messages, toMsgView(m, canShare))
		}
		if canShare && h.ListChannels != nil {
			d.Channels = h.ListChannels(r.Context(), h.cid(r.Context()))
		}
	}
	active := ""
	if hasActive {
		active = t.ID
	}
	for _, th := range threads {
		d.Threads = append(d.Threads, webtempl.AgentThreadView{
			ID: th.ID, Title: th.Title, Visibility: th.Visibility, Active: th.ID == active,
		})
	}
	return d
}

// GetStream (/agent/{thread}/stream) is the per-thread SSE: refetch + fat-morph
// on every broadcast, exactly like chat. The 100ms-cadence broadcasts come
// from the generation runner.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	t, _, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	sse := render.NewSSE(w, r)

	// Initial sync on every (re)connection — restores the conversation after a
	// tab sleep / refresh and, if a generation is in flight, picks it up live.
	if err := h.streamMorph(r.Context(), sse, t.ID); err != nil {
		return
	}

	local, unsubscribe := h.Bus.Subscribe(t.ID)
	defer unsubscribe()

	var natsCh chan *nats.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *nats.Msg, 32)
		sub, err := h.NATS.ChanSubscribe(natsx.AgentThreadSubject(h.cid(r.Context()), t.ID), natsCh)
		if err == nil {
			defer sub.Unsubscribe()
		} else {
			natsCh = nil
		}
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-local:
		case _, alive := <-natsCh:
			if !alive {
				natsCh = nil
				continue
			}
		}
		if err := h.streamMorph(r.Context(), sse, t.ID); err != nil {
			return
		}
	}
}

func (h *Handler) streamMorph(ctx context.Context, sse *datastar.ServerSentEventGenerator, tid string) error {
	msgs, err := h.Repo.Messages(ctx, tid)
	if err != nil {
		return nil // transient; keep the stream open
	}
	return h.fatMorph(sse, h.cslug(ctx), tid, msgs)
}

// fatMorph rewrites #agent-messages, re-fires the scroll anchor, and syncs the
// generating signal so the composer flips between Send and Stop.
func (h *Handler) fatMorph(sse *datastar.ServerSentEventGenerator, slug, tid string, msgs []Message) error {
	canShare := h.ShareToChannel != nil
	views := make([]webtempl.AgentMsgView, 0, len(msgs))
	for _, m := range msgs {
		views = append(views, toMsgView(m, canShare))
	}
	if err := sse.PatchElementTempl(webtempl.AgentMessages(slug, tid, views), datastar.WithModeOuter()); err != nil {
		return err
	}
	if err := sse.PatchElementTempl(webtempl.AgentScrollAnchor(), datastar.WithModeReplace()); err != nil {
		return err
	}
	if h.threadGenerating(tid, msgs) {
		return sse.PatchSignals([]byte(`{"agent_generating":true}`))
	}
	return sse.PatchSignals([]byte(`{"agent_generating":false}`))
}

func (h *Handler) threadGenerating(tid string, msgs []Message) bool {
	if h.Runner.IsRunning(tid) {
		return true
	}
	for _, m := range msgs {
		if m.Status == StatusGenerating {
			return true
		}
	}
	return false
}

// --- writes ---------------------------------------------------------------

type newSignals struct {
	Body       string `json:"agent_body"`
	Visibility string `json:"agent_visibility"`
}

// PostNew (/agent/new) mints a thread and, when the composer carried a prompt,
// sends the first turn immediately, then navigates to the thread.
func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in newSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	cid := h.cid(r.Context())
	cfg, _ := h.Repo.GetConfig(r.Context(), cid)
	t, err := h.Svc.CreateThread(r.Context(), cid, id.User.ID, in.Visibility, cfg.Model)
	if err != nil {
		h.Log.Error("agent: create thread", "err", err)
		http.Error(w, "create thread", http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	if body := strings.TrimSpace(in.Body); body != "" && cfg.Enabled {
		h.startSend(r.Context(), t, id.User.ID, body, cfg)
	}
	_ = sse.PatchSignals([]byte(`{"agent_body":""}`))
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/agent/" + t.ID)
}

type sendSignals struct {
	Body string `json:"agent_body"`
}

// PostSend (/agent/{thread}/send) appends the member's turn and kicks the
// generation runner.
func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	t, id, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	var in sendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(in.Body)
	sse := render.NewSSE(w, r)
	if body == "" || len(body) > 8000 {
		return
	}
	cfg, _ := h.Repo.GetConfig(r.Context(), h.cid(r.Context()))
	if !cfg.Enabled {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("AI is not enabled for this community yet. An admin can turn it on in Admin → AI."))
		return
	}
	if h.Runner.IsRunning(t.ID) {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("Still answering the previous message — hang on."))
		return
	}
	h.startSend(r.Context(), t, id.User.ID, body, cfg)
	// Immediately reflect the new user turn + empty assistant bubble to the
	// sender; their own SSE stream also receives the broadcast.
	if msgs, err := h.Repo.Messages(r.Context(), t.ID); err == nil {
		_ = h.fatMorph(sse, h.cslug(r.Context()), t.ID, msgs)
	}
	_ = sse.PatchSignals([]byte(`{"agent_body":""}`))
}

// startSend persists the turn + placeholder and launches the runner. It also
// fires an immediate broadcast so other open streams render the user turn
// before the first token lands.
func (h *Handler) startSend(ctx context.Context, t Thread, authorID, body string, cfg Config) {
	asstID, history, err := h.Svc.Send(ctx, t, authorID, body)
	if err != nil {
		h.Log.Error("agent: send", "err", err)
		return
	}
	h.broadcast(ctx, t.ID)
	if err := h.Runner.Start(t.CommunityID, t.ID, asstID, cfg, history); err != nil {
		h.Log.Warn("agent: start generation", "thread", t.ID, "err", err)
	}
}

// PostStop (/agent/{thread}/stop) cancels an in-flight generation.
func (h *Handler) PostStop(w http.ResponseWriter, r *http.Request) {
	t, _, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	h.Runner.Stop(t.ID)
	sse := render.NewSSE(w, r)
	if msgs, err := h.Repo.Messages(r.Context(), t.ID); err == nil {
		_ = h.fatMorph(sse, h.cslug(r.Context()), t.ID, msgs)
	}
}

// PostRegenerate (/agent/{thread}/regenerate) re-runs the last assistant turn.
func (h *Handler) PostRegenerate(w http.ResponseWriter, r *http.Request) {
	t, _, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	sse := render.NewSSE(w, r)
	cfg, _ := h.Repo.GetConfig(r.Context(), h.cid(r.Context()))
	if !cfg.Enabled || h.Runner.IsRunning(t.ID) {
		return
	}
	asstID, history, err := h.Svc.Regenerate(r.Context(), t.ID)
	if err != nil {
		return
	}
	if err := h.Runner.Start(t.CommunityID, t.ID, asstID, cfg, history); err != nil {
		h.Log.Warn("agent: regenerate", "thread", t.ID, "err", err)
	}
	h.broadcast(r.Context(), t.ID)
	if msgs, err := h.Repo.Messages(r.Context(), t.ID); err == nil {
		_ = h.fatMorph(sse, h.cslug(r.Context()), t.ID, msgs)
	}
}

// PostDelete (/agent/{thread}/delete) removes a thread the viewer owns (or, for
// shared threads, that an admin moderates).
func (h *Handler) PostDelete(w http.ResponseWriter, r *http.Request) {
	t, id, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	if !canDelete(t, id.User.ID, id.Membership.Role.AtLeast(auth.RoleAdmin)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	h.Runner.Stop(t.ID)
	_ = h.Repo.DeleteThread(r.Context(), t.ID)
	sse := render.NewSSE(w, r)
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/agent")
}

type shareSignals struct {
	MsgID   string `json:"agent_share_msg_id"`
	Channel string `json:"agent_share_channel"`
}

// PostShareToChannel (/agent/{thread}/share) copies an assistant answer into a
// community chat channel as the requesting member.
func (h *Handler) PostShareToChannel(w http.ResponseWriter, r *http.Request) {
	t, id, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	var in shareSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if h.ShareToChannel == nil {
		return
	}
	msg, err := h.Repo.MessageByID(r.Context(), strings.TrimSpace(in.MsgID))
	if err != nil || msg.ThreadID != t.ID || msg.Role != RoleAssistant || strings.TrimSpace(msg.BodyMD) == "" {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("Nothing to share."))
		return
	}
	channel := strings.TrimSpace(in.Channel)
	if channel == "" {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("Pick a channel first."))
		return
	}
	body := "🤖 **From the agent**\n\n" + msg.BodyMD
	name, err := h.ShareToChannel(r.Context(), h.cid(r.Context()), channel, id.User.ID, body)
	if err != nil {
		h.Log.Warn("agent: share to channel", "err", err)
		_ = sse.PatchElementTempl(webtempl.AgentNotice("Couldn't share to that channel."))
		return
	}
	_ = sse.PatchSignals([]byte(`{"_agent_share_open":false,"agent_share_msg_id":"","agent_share_channel":""}`))
	_ = sse.PatchElementTempl(webtempl.AgentNotice("Shared to #" + name + "."))
}

// --- admin config ---------------------------------------------------------

// GetConfig (/admin/ai) renders the per-community AI settings form.
func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.Repo.GetConfig(r.Context(), h.cid(r.Context()))
	if err != nil {
		http.Error(w, "load config", http.StatusInternalServerError)
		return
	}
	_ = webtempl.AgentConfigPage(h.viewer(r), h.cslug(r.Context()), toConfigView(cfg)).Render(r.Context(), w)
}

type configSignals struct {
	Provider     string `json:"ai_provider"`
	BaseURL      string `json:"ai_base_url"`
	Model        string `json:"ai_model"`
	APIKey       string `json:"ai_api_key"`
	SystemPrompt string `json:"ai_system_prompt"`
	Enabled      bool   `json:"ai_enabled"`
}

// PostSaveConfig (/admin/ai) persists the settings and re-renders the form.
func (h *Handler) PostSaveConfig(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in configSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	cur, _ := h.Repo.GetConfig(r.Context(), h.cid(r.Context()))
	cur.CommunityID = h.cid(r.Context())
	cur.Provider = strings.TrimSpace(in.Provider)
	if cur.Provider == "" {
		cur.Provider = ProviderOllama
	}
	cur.BaseURL = strings.TrimSpace(in.BaseURL)
	cur.Model = strings.TrimSpace(in.Model)
	cur.SystemPrompt = in.SystemPrompt
	cur.Enabled = in.Enabled
	cur.UpdatedBy = id.User.ID
	// Only overwrite the stored key when a new one was typed (the form never
	// echoes the existing secret back).
	if k := strings.TrimSpace(in.APIKey); k != "" {
		cur.APIKeyEnc = k
	}
	sse := render.NewSSE(w, r)
	if err := h.Repo.SaveConfig(r.Context(), cur); err != nil {
		h.Log.Error("agent: save config", "err", err)
		_ = sse.PatchElementTempl(webtempl.AgentConfigForm(h.cslug(r.Context()), toConfigView(cur), "Couldn't save — try again."))
		return
	}
	_ = sse.PatchElementTempl(webtempl.AgentConfigForm(h.cslug(r.Context()), toConfigView(cur), "Saved."))
}

func (h *Handler) broadcast(ctx context.Context, tid string) {
	if h.Bus != nil {
		h.Bus.Broadcast(tid)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.AgentThreadSubject(h.cid(ctx), tid), []byte(tid))
	}
}

// --- view mapping ---------------------------------------------------------

func toMsgView(m Message, canShare bool) webtempl.AgentMsgView {
	v := webtempl.AgentMsgView{
		ID:          m.ID,
		Role:        m.Role,
		BodyHTML:    m.BodyHTML,
		Status:      m.Status,
		Error:       m.Error,
		IsUser:      m.Role == RoleUser,
		IsAssistant: m.Role == RoleAssistant,
		Generating:  m.Status == StatusGenerating,
		Interrupted: m.Status == StatusInterrupted,
		Errored:     m.Status == StatusError,
	}
	v.CanShare = canShare && v.IsAssistant && m.Status == StatusDone && strings.TrimSpace(m.BodyMD) != ""
	return v
}

func toConfigView(c Config) webtempl.AgentConfigView {
	return webtempl.AgentConfigView{
		Provider:     c.Provider,
		BaseURL:      c.BaseURL,
		Model:        c.Model,
		SystemPrompt: c.SystemPrompt,
		Enabled:      c.Enabled,
		APIKeySet:    strings.TrimSpace(c.APIKeyEnc) != "",
	}
}
