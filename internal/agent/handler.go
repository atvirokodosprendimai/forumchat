package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"
	datastar "github.com/starfederation/datastar-go/datastar"
)

// AgentImageMaxBytes caps an attached image (pre-base64). Vision agents only.
const AgentImageMaxBytes = 16 << 20

// Handler is the HTTP boundary for the Agent feature.
type Handler struct {
	Repo    *Repo
	Svc     *Service
	Runner  *Runner
	Bus     *Bus
	NATS    *nats.Conn
	Uploads *uploads.Store
	Log     *slog.Logger

	CommunityID   string
	CommunityName string

	// ShareToChannel posts an assistant answer into a community chat channel
	// as the requesting member. Wired in main.go to chat's send+broadcast so
	// this package needn't import chat. Nil disables the share affordance.
	ShareToChannel func(ctx context.Context, communityID, channelSlug, authorID, bodyMD string) (channelName string, err error)
	// ListChannels lists the community's chat channels for the share dropdown.
	ListChannels func(ctx context.Context, communityID string) []webtempl.ChannelView
	// SearchExternalRefs backs the $-reference autocomplete's non-agent results
	// (forum subjects, projects, issues, discussions). Closure (wired in
	// main.go) so this package needn't import forum/projects. Nil → only agent
	// threads are searched.
	SearchExternalRefs func(ctx context.Context, communityID, q string, limit int) []webtempl.AgentRefView
	// ResolveRef loads the textual content of a $-referenced item (a thread's
	// conversation, a forum thread, etc.) so it can be expanded into the model
	// prompt. Wired in main.go. Returns ok=false when not resolvable.
	ResolveRef func(ctx context.Context, communityID, kind, id string) (title, content string, ok bool)
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
// creator. Writes a 404 (not 403, so a private thread's existence isn't
// revealed) and returns ok=false otherwise.
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

// threadAgent resolves a thread's pinned agent, falling back to the first
// enabled agent for legacy threads with no pin.
func (h *Handler) threadAgent(ctx context.Context, t Thread) (Agent, error) {
	if t.AgentID != "" {
		if a, err := h.Repo.AgentByID(ctx, t.AgentID); err == nil {
			return a, nil
		}
	}
	enabled, _ := h.Repo.ListEnabledAgents(ctx, t.CommunityID)
	if len(enabled) == 0 {
		return Agent{}, ErrDisabled
	}
	return enabled[0], nil
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
	_ = webtempl.AgentPage(h.pageData(r, Thread{}, false, nil, threads)).Render(r.Context(), w)
}

// GetPage (/agent/{thread}) renders the two-pane page for one thread.
func (h *Handler) GetPage(w http.ResponseWriter, r *http.Request) {
	t, id, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	threads, _ := h.Repo.ListThreads(r.Context(), h.cid(r.Context()), id.User.ID)
	msgs, _ := h.Repo.Messages(r.Context(), t.ID)
	_ = webtempl.AgentPage(h.pageData(r, t, true, msgs, threads)).Render(r.Context(), w)
}

func (h *Handler) pageData(r *http.Request, t Thread, hasActive bool, msgs []Message, threads []Thread) webtempl.AgentPageData {
	ctx := r.Context()
	slug := h.cslug(ctx)
	canShare := h.ShareToChannel != nil
	enabled, _ := h.Repo.ListEnabledAgents(ctx, h.cid(ctx))

	d := webtempl.AgentPageData{
		Viewer:          h.viewer(r),
		Slug:            slug,
		HasActive:       hasActive,
		HasEnabledAgent: len(enabled) > 0,
		CanShare:        canShare,
	}
	for _, a := range enabled {
		d.Agents = append(d.Agents, webtempl.AgentOptionView{ID: a.ID, Name: a.Name, Vision: a.Vision})
		if a.Vision {
			d.AnyVision = true
		}
	}
	if hasActive {
		d.Active = webtempl.AgentThreadView{ID: t.ID, Title: t.Title, Visibility: t.Visibility}
		if a, err := h.threadAgent(ctx, t); err == nil {
			d.ActiveAgentName = a.Name
			d.ActiveAgentID = a.ID
			d.ActiveVision = a.Vision
		}
		d.Generating = h.threadGenerating(t.ID, msgs)
		for _, m := range msgs {
			d.Messages = append(d.Messages, toMsgView(m, canShare))
		}
		if canShare && h.ListChannels != nil {
			d.Channels = h.ListChannels(ctx, h.cid(ctx))
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
// on every broadcast. The 100ms-cadence broadcasts come from the runner.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	t, _, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	sse := render.NewSSE(w, r)

	// Subscribe BEFORE the initial render so a broadcast that lands between the
	// first paint and the subscription isn't lost (the buffered channel holds
	// it). Otherwise a generation that started just before this connection
	// could go un-rendered until the next event.
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

	if err := h.streamMorph(r.Context(), sse, t.ID); err != nil {
		return
	}

	// Safety re-sync: while a generation is in flight on this node, re-render
	// every interval even if no bus/NATS wake arrives. Event-driven stays the
	// fast path (100ms via the runner); this is a belt-and-braces fallback so a
	// freshly-connected stream can never get stuck on the "thinking" dots.
	resync := time.NewTicker(400 * time.Millisecond)
	defer resync.Stop()

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
		case <-resync.C:
			if !h.Runner.IsRunning(t.ID) {
				continue // idle thread — don't re-render for nothing
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
		return nil
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

type refSignals struct {
	Q string `json:"agent_ref_query"`
}

// GetRefSearch (/agent/refs) powers the $-reference autocomplete: it matches
// the typed query against agent thread titles + forum subjects and patches the
// dropdown. Read-only.
func (h *Handler) GetRefSearch(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in refSignals
	_ = datastar.ReadSignals(r, &in)
	q := strings.TrimSpace(in.Q)
	sse := render.NewSSE(w, r)
	slug := h.cslug(r.Context())
	if len(q) < 2 {
		_ = sse.PatchElementTempl(webtempl.AgentRefResults(slug, nil))
		return
	}
	cid := h.cid(r.Context())
	var refs []webtempl.AgentRefView
	if threads, err := h.Repo.SearchThreads(r.Context(), cid, id.User.ID, q, 6); err == nil {
		for _, t := range threads {
			refs = append(refs, webtempl.AgentRefView{Kind: "agent", ID: t.ID, Title: t.Title})
		}
	}
	if h.SearchExternalRefs != nil {
		refs = append(refs, h.SearchExternalRefs(r.Context(), cid, q, 6)...)
	}
	_ = sse.PatchElementTempl(webtempl.AgentRefResults(slug, refs))
}

// --- writes ---------------------------------------------------------------

type newSignals struct {
	Body       string `json:"agent_body"`
	Visibility string `json:"agent_visibility"`
	AgentID    string `json:"agent_pick"`
	ImageData  string `json:"agent_image_data"`
	Refs       string `json:"agent_refs"`
}

// pickAgent resolves the agent to use for a new thread: the explicitly chosen
// one (validated against this community + enabled), else the first enabled.
func (h *Handler) pickAgent(ctx context.Context, agentID string) (Agent, bool) {
	cid := h.cid(ctx)
	if agentID != "" {
		if a, err := h.Repo.AgentByID(ctx, agentID); err == nil && a.CommunityID == cid && a.Enabled {
			return a, true
		}
	}
	enabled, _ := h.Repo.ListEnabledAgents(ctx, cid)
	if len(enabled) == 0 {
		return Agent{}, false
	}
	return enabled[0], true
}

// PostNew (/agent/new) mints a thread (with a chosen agent) and, when the
// composer carried a prompt/image, sends the first turn, then navigates.
func (h *Handler) PostNew(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, AgentImageMaxBytes+1<<20)
	var in newSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	a, ok := h.pickAgent(r.Context(), in.AgentID)
	if !ok {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("No AI agent is enabled yet."))
		return
	}
	t, err := h.Svc.CreateThread(r.Context(), h.cid(r.Context()), id.User.ID, a, in.Visibility)
	if err != nil {
		h.Log.Error("agent: create thread", "err", err)
		http.Error(w, "create thread", http.StatusInternalServerError)
		return
	}
	body := strings.TrimSpace(in.Body)
	images := h.stageImage(r.Context(), id.User.ID, a, in.ImageData, &body)
	if body != "" || len(images) > 0 {
		h.startSend(r.Context(), t, a, id.User.ID, body, images, h.expandRefs(r.Context(), t.CommunityID, in.Refs))
	}
	_ = sse.PatchSignals([]byte(`{"agent_body":"","agent_image_data":"","agent_refs":""}`))
	_ = sse.Redirect("/c/" + h.cslug(r.Context()) + "/agent/" + t.ID)
}

type sendSignals struct {
	Body      string `json:"agent_body"`
	ImageData string `json:"agent_image_data"`
	Refs      string `json:"agent_refs"`
}

// expandRefs turns the composer's agent_refs JSON into a context block that is
// prepended to the model prompt (only — the displayed message keeps the clean
// $Title). Each ref is resolved to its thread/forum content via ResolveRef;
// unresolvable ones fall back to a title marker.
func (h *Handler) expandRefs(ctx context.Context, communityID, refsJSON string) string {
	if h.ResolveRef == nil || strings.TrimSpace(refsJSON) == "" {
		return ""
	}
	var refs []struct {
		Kind  string `json:"kind"`
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(refsJSON), &refs); err != nil {
		return ""
	}
	var b strings.Builder
	for _, ref := range refs {
		title, content, ok := h.ResolveRef(ctx, communityID, ref.Kind, ref.ID)
		if title == "" {
			title = ref.Title
		}
		if !ok || strings.TrimSpace(content) == "" {
			if title != "" {
				b.WriteString("Referenced " + ref.Kind + ": " + title + "\n\n")
			}
			continue
		}
		b.WriteString("===== Referenced " + ref.Kind + ": " + title + " =====\n")
		b.WriteString(strings.TrimSpace(content))
		b.WriteString("\n===== end =====\n\n")
	}
	return strings.TrimSpace(b.String())
}

// PostSend (/agent/{thread}/send) appends the member's turn, kicks the runner,
// and STREAMS the whole generation back through THIS response. Streaming via
// the POST response (rather than relying on the separate persistent /stream)
// is deliberate: the browser tears the persistent SSE down when this POST
// fires, so the sender's live tokens must ride the POST itself. The runner
// keeps filling the DB regardless, so a navigate-away/refresh still resumes via
// GetStream.
func (h *Handler) PostSend(w http.ResponseWriter, r *http.Request) {
	t, id, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, AgentImageMaxBytes+1<<20)
	var in sendSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	body := strings.TrimSpace(in.Body)
	sse := render.NewSSE(w, r)
	a, err := h.threadAgent(r.Context(), t)
	if err != nil || !a.Enabled {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("This agent isn't available right now."))
		return
	}
	images := h.stageImage(r.Context(), id.User.ID, a, in.ImageData, &body)
	if (body == "" && len(images) == 0) || len(body) > 8000 {
		return
	}
	if h.Runner.IsRunning(t.ID) {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("Still answering the previous message — hang on."))
		return
	}

	// Subscribe before kicking the runner so no early flush is missed.
	local, unsubscribe := h.Bus.Subscribe(t.ID)
	defer unsubscribe()

	refContext := h.expandRefs(r.Context(), t.CommunityID, in.Refs)
	h.startSend(r.Context(), t, a, id.User.ID, body, images, refContext)

	// Clear the composer immediately — including the (large) image data URL so
	// it never lingers in the signal bag.
	_ = sse.PatchSignals([]byte(`{"agent_body":"","agent_image_data":"","agent_refs":""}`))
	if msgs, err := h.Repo.Messages(r.Context(), t.ID); err == nil {
		if err := h.fatMorph(sse, h.cslug(r.Context()), t.ID, msgs); err != nil {
			return
		}
	}
	h.streamUntilDone(r, sse, t.ID, local)
}

// streamUntilDone re-renders the thread on every bus wake (and a 150ms safety
// tick) until the generation completes or the client disconnects. Shared by
// PostSend and PostRegenerate so the actor sees streaming through their own
// request.
func (h *Handler) streamUntilDone(r *http.Request, sse *datastar.ServerSentEventGenerator, tid string, local <-chan struct{}) {
	tick := time.NewTicker(120 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-local:
		case <-tick.C:
		}
		msgs, err := h.Repo.Messages(r.Context(), tid)
		if err != nil {
			continue
		}
		if h.threadGenerating(tid, msgs) {
			// Incremental: morph ONLY the growing assistant bubble (by id), not
			// the whole conversation — so the list above doesn't repaint and
			// flicker on every token batch.
			if err := h.streamBubble(sse, h.cslug(r.Context()), tid, msgs); err != nil {
				return
			}
			continue
		}
		// Finished: one full morph settles status/actions and flips the signal.
		_ = h.fatMorph(sse, h.cslug(r.Context()), tid, msgs)
		return
	}
}

// streamBubble morphs just the last assistant message (the one being streamed)
// plus re-fires the scroll anchor. Used during active generation to avoid
// repainting the entire message list.
func (h *Handler) streamBubble(sse *datastar.ServerSentEventGenerator, slug, tid string, msgs []Message) error {
	var last Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant {
			last = msgs[i]
			break
		}
	}
	if last.ID == "" {
		return nil
	}
	if err := sse.PatchElementTempl(webtempl.AgentBubble(slug, tid, toMsgView(last, h.ShareToChannel != nil)), datastar.WithModeOuter()); err != nil {
		return err
	}
	return sse.PatchElementTempl(webtempl.AgentScrollAnchor(), datastar.WithModeReplace())
}

// stageImage handles a vision agent's attached image: it uploads it (so the
// bubble shows a small signed URL, not a giant data: URL re-sent every morph)
// AND returns the raw base64 for the model. Non-vision agents / no image → nil.
// On success it prepends a markdown image to *body for display.
func (h *Handler) stageImage(ctx context.Context, userID string, a Agent, dataURL string, body *string) []string {
	dataURL = strings.TrimSpace(dataURL)
	if dataURL == "" || !a.Vision || h.Uploads == nil {
		return nil
	}
	_, b64, ok := dataURLBase64(dataURL)
	if !ok {
		return nil
	}
	u, err := h.Uploads.SaveDataURL(ctx, userID, a.CommunityID, dataURL, AgentImageMaxBytes)
	if err != nil {
		h.Log.Warn("agent: stage image", "err", err)
		return nil
	}
	url := h.Uploads.SignedURL(u.ID, userID, 24*time.Hour)
	img := "[![](" + url + ")](" + url + ")"
	if *body == "" {
		*body = img
	} else {
		*body = img + "\n\n" + *body
	}
	return []string{b64}
}

// startSend persists the turn + placeholder and launches the runner. refContext
// (expanded $-references) is prepended to the user turn sent to the MODEL only —
// the stored/displayed message keeps the clean text + $Title chunks.
func (h *Handler) startSend(ctx context.Context, t Thread, a Agent, authorID, body string, images []string, refContext string) {
	asstID, history, err := h.Svc.Send(ctx, t, authorID, body, images)
	if err != nil {
		h.Log.Error("agent: send", "err", err)
		return
	}
	if refContext != "" {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Role == RoleUser {
				history[i].Content = "Use the referenced material below as context for the user's message.\n\n" +
					refContext + "\n\n----- User's message -----\n" + history[i].Content
				break
			}
		}
	}
	h.broadcast(ctx, t.ID)
	if err := h.Runner.Start(t.CommunityID, t.ID, asstID, a, history); err != nil {
		h.Log.Warn("agent: start generation", "thread", t.ID, "err", err)
	}
}

type switchSignals struct {
	AgentID string `json:"agent_active"`
}

// PostSetAgent (/agent/{thread}/agent) switches the thread's agent (model +
// system prompt) mid-conversation. The history stays; the next turn uses the
// new agent. Re-renders the composer so its vision attach matches.
func (h *Handler) PostSetAgent(w http.ResponseWriter, r *http.Request) {
	t, _, ok := h.resolveReadable(w, r)
	if !ok {
		return
	}
	var in switchSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	a, err := h.Repo.AgentByID(r.Context(), strings.TrimSpace(in.AgentID))
	if err != nil || a.CommunityID != h.cid(r.Context()) || !a.Enabled {
		return
	}
	if err := h.Repo.SetThreadAgent(r.Context(), t.ID, a.ID, a.Model); err != nil {
		h.Log.Warn("agent: switch agent", "thread", t.ID, "err", err)
		return
	}
	// Reflect the new agent's vision capability in the composer immediately.
	_ = sse.PatchElementTempl(webtempl.AgentComposer(h.cslug(r.Context()), t.ID, a.Vision))
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
	a, err := h.threadAgent(r.Context(), t)
	if err != nil || !a.Enabled || h.Runner.IsRunning(t.ID) {
		return
	}
	local, unsubscribe := h.Bus.Subscribe(t.ID)
	defer unsubscribe()
	asstID, history, err := h.Svc.Regenerate(r.Context(), t.ID)
	if err != nil {
		return
	}
	if err := h.Runner.Start(t.CommunityID, t.ID, asstID, a, history); err != nil {
		h.Log.Warn("agent: regenerate", "thread", t.ID, "err", err)
	}
	if msgs, err := h.Repo.Messages(r.Context(), t.ID); err == nil {
		if err := h.fatMorph(sse, h.cslug(r.Context()), t.ID, msgs); err != nil {
			return
		}
	}
	h.streamUntilDone(r, sse, t.ID, local)
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

// --- admin: manage agents -------------------------------------------------

// GetAgents (/admin/ai) lists the community's agents with editor + add form.
func (h *Handler) GetAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := h.Repo.ListAgents(r.Context(), h.cid(r.Context()))
	if err != nil {
		http.Error(w, "load agents", http.StatusInternalServerError)
		return
	}
	views := make([]webtempl.AgentAdminView, 0, len(agents))
	for _, a := range agents {
		views = append(views, toAdminView(a))
	}
	servers, _ := h.Repo.ListMCPServers(r.Context(), h.cid(r.Context()))
	_ = webtempl.AgentAdminPage(h.viewer(r), h.cslug(r.Context()), views, toMCPViews(servers)).Render(r.Context(), w)
}

type mcpSignals struct {
	Name      string `json:"mcp_name"`
	Transport string `json:"mcp_transport"`
	Command   string `json:"mcp_command"`
	Args      string `json:"mcp_args"`
	URL       string `json:"mcp_url"`
	Headers   string `json:"mcp_headers"`
	Env       string `json:"mcp_env"`
}

// PostSaveMCPServer (/admin/ai/mcp) connects a new external MCP server.
func (h *Handler) PostSaveMCPServer(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in mcpSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	m := MCPServer{
		CommunityID: h.cid(r.Context()),
		Name:        in.Name,
		Transport:   in.Transport,
		Command:     in.Command,
		Args:        strings.Fields(in.Args),
		URL:         in.URL,
		Headers:     parseKVLines(in.Headers, ":"),
		Env:         parseKVLines(in.Env, "="),
		Enabled:     true,
		UpdatedBy:   id.User.ID,
	}
	sse := render.NewSSE(w, r)
	if _, err := h.Svc.SaveMCPServer(r.Context(), m); err != nil {
		_ = sse.PatchElementTempl(webtempl.AgentMCPStatus(mcpSaveErr(err)))
		return
	}
	h.renderMCPList(r.Context(), sse)
	_ = sse.PatchSignals([]byte(`{"mcp_name":"","mcp_command":"","mcp_args":"","mcp_url":"","mcp_headers":"","mcp_env":""}`))
	_ = sse.PatchElementTempl(webtempl.AgentMCPStatus("Connected."))
}

// PostToggleMCPServer (/admin/ai/mcp/{id}/toggle) flips a server enabled/disabled.
func (h *Handler) PostToggleMCPServer(w http.ResponseWriter, r *http.Request) {
	m, err := h.Repo.MCPServerByID(r.Context(), chi.URLParam(r, "id"))
	sse := render.NewSSE(w, r)
	if err != nil || m.CommunityID != h.cid(r.Context()) {
		return
	}
	_ = h.Repo.SetMCPServerEnabled(r.Context(), m.ID, !m.Enabled)
	h.renderMCPList(r.Context(), sse)
}

// PostDeleteMCPServer (/admin/ai/mcp/{id}/delete) disconnects a server.
func (h *Handler) PostDeleteMCPServer(w http.ResponseWriter, r *http.Request) {
	m, err := h.Repo.MCPServerByID(r.Context(), chi.URLParam(r, "id"))
	sse := render.NewSSE(w, r)
	if err != nil || m.CommunityID != h.cid(r.Context()) {
		return
	}
	_ = h.Repo.DeleteMCPServer(r.Context(), m.ID)
	h.renderMCPList(r.Context(), sse)
}

func (h *Handler) renderMCPList(ctx context.Context, sse *datastar.ServerSentEventGenerator) {
	servers, _ := h.Repo.ListMCPServers(ctx, h.cid(ctx))
	_ = sse.PatchElementTempl(webtempl.AgentMCPList(h.cslug(ctx), toMCPViews(servers)))
}

func toMCPViews(servers []MCPServer) []webtempl.MCPServerView {
	out := make([]webtempl.MCPServerView, 0, len(servers))
	for _, m := range servers {
		target := m.URL
		if m.Transport == MCPTransportStdio {
			target = strings.TrimSpace(m.Command + " " + strings.Join(m.Args, " "))
		}
		out = append(out, webtempl.MCPServerView{
			ID: m.ID, Name: m.Name, Transport: m.Transport, Target: target, Enabled: m.Enabled,
		})
	}
	return out
}

func mcpSaveErr(err error) string {
	switch err {
	case ErrMCPName:
		return "Name is required."
	case ErrMCPTransport:
		return "Transport must be stdio or http."
	case ErrMCPTarget:
		return "stdio needs a command; http needs a URL."
	case ErrMCPCap:
		return "Too many MCP servers."
	default:
		return "Couldn't connect — check the details and try again."
	}
}

// parseKVLines parses "key<sep>value" lines into a map, trimming whitespace and
// skipping blanks. Used for HTTP headers (":") and stdio env ("=").
func parseKVLines(s, sep string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, sep)
		k = strings.TrimSpace(k)
		if !found || k == "" {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GetNewAgentForm (/admin/ai/new) patches a blank editor form.
func (h *Handler) GetNewAgentForm(w http.ResponseWriter, r *http.Request) {
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.AgentAdminForm(h.cslug(r.Context()), webtempl.AgentAdminView{}, ""))
}

// GetEditAgentForm (/admin/ai/{id}/edit) patches the editor form for one agent.
func (h *Handler) GetEditAgentForm(w http.ResponseWriter, r *http.Request) {
	a, err := h.Repo.AgentByID(r.Context(), chi.URLParam(r, "id"))
	sse := render.NewSSE(w, r)
	if err != nil || a.CommunityID != h.cid(r.Context()) {
		_ = sse.PatchElementTempl(webtempl.AgentNotice("Agent not found."))
		return
	}
	_ = sse.PatchElementTempl(webtempl.AgentAdminForm(h.cslug(r.Context()), toAdminView(a), ""))
}

type agentSignals struct {
	AgentID      string `json:"ai_agent_id"`
	Name         string `json:"ai_name"`
	Provider     string `json:"ai_provider"`
	BaseURL      string `json:"ai_base_url"`
	Model        string `json:"ai_model"`
	APIKey       string `json:"ai_api_key"`
	SystemPrompt string `json:"ai_system_prompt"`
	Vision       bool   `json:"ai_vision"`
	ToolsEnabled bool   `json:"ai_tools"`
	Enabled      bool   `json:"ai_enabled"`
}

// PostSaveAgent (/admin/ai) creates or updates an agent, then re-renders the
// list and resets the form.
func (h *Handler) PostSaveAgent(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in agentSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	cid := h.cid(r.Context())
	a := Agent{
		ID: strings.TrimSpace(in.AgentID), CommunityID: cid, Name: in.Name,
		Provider: in.Provider, BaseURL: in.BaseURL, Model: in.Model,
		SystemPrompt: in.SystemPrompt, Vision: in.Vision, ToolsEnabled: in.ToolsEnabled, Enabled: in.Enabled,
		UpdatedBy: id.User.ID,
	}
	// Preserve the stored key unless a new one was typed (the form never echoes
	// the secret); load the existing row when editing.
	if a.ID != "" {
		if cur, err := h.Repo.AgentByID(r.Context(), a.ID); err == nil && cur.CommunityID == cid {
			a.APIKeyEnc = cur.APIKeyEnc
		}
	}
	if k := strings.TrimSpace(in.APIKey); k != "" {
		a.APIKeyEnc = k
	}

	sse := render.NewSSE(w, r)
	if _, err := h.Svc.SaveAgent(r.Context(), a); err != nil {
		_ = sse.PatchElementTempl(webtempl.AgentAdminForm(h.cslug(r.Context()), toAdminView(a), saveErr(err)))
		return
	}
	h.broadcast(r.Context(), "") // wake any agent pages so the picker refreshes
	h.renderAdminList(r.Context(), sse)
	_ = sse.PatchElementTempl(webtempl.AgentAdminForm(h.cslug(r.Context()), webtempl.AgentAdminView{}, "Saved."))
}

// PostDeleteAgent (/admin/ai/{id}/delete) removes an agent (threads cascade).
func (h *Handler) PostDeleteAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.Repo.AgentByID(r.Context(), chi.URLParam(r, "id"))
	sse := render.NewSSE(w, r)
	if err != nil || a.CommunityID != h.cid(r.Context()) {
		return
	}
	_ = h.Repo.DeleteAgent(r.Context(), a.ID)
	h.renderAdminList(r.Context(), sse)
}

func (h *Handler) renderAdminList(ctx context.Context, sse *datastar.ServerSentEventGenerator) {
	agents, _ := h.Repo.ListAgents(ctx, h.cid(ctx))
	views := make([]webtempl.AgentAdminView, 0, len(agents))
	for _, a := range agents {
		views = append(views, toAdminView(a))
	}
	_ = sse.PatchElementTempl(webtempl.AgentAdminList(h.cslug(ctx), views))
}

func saveErr(err error) string {
	switch err {
	case ErrNoName:
		return "Name is required."
	case ErrAgentCap:
		return "Too many agents."
	default:
		return "Couldn't save — try again."
	}
}

func (h *Handler) broadcast(ctx context.Context, tid string) {
	if h.Bus != nil {
		h.Bus.Broadcast(tid)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.AgentThreadSubject(h.cid(ctx), tid), []byte(tid))
	}
}

// --- helpers --------------------------------------------------------------

// dataURLBase64 splits a "data:<mime>;base64,<payload>" URL into its mime and
// raw base64 payload (Ollama's images shape). ok=false for anything else.
func dataURLBase64(dataURL string) (mime, b64 string, ok bool) {
	if !strings.HasPrefix(dataURL, "data:") {
		return "", "", false
	}
	i := strings.IndexByte(dataURL, ',')
	if i < 0 {
		return "", "", false
	}
	meta := dataURL[len("data:"):i]
	if !strings.Contains(meta, "base64") {
		return "", "", false
	}
	mime = meta
	if j := strings.IndexByte(mime, ';'); j >= 0 {
		mime = mime[:j]
	}
	b64 = dataURL[i+1:]
	return mime, b64, b64 != ""
}

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
	for _, tc := range m.ToolCalls {
		v.ToolCalls = append(v.ToolCalls, webtempl.AgentToolView{
			Server: tc.Server, Name: tc.Name, Args: tc.Args, Result: tc.Result, OK: tc.OK,
		})
	}
	return v
}

func toAdminView(a Agent) webtempl.AgentAdminView {
	return webtempl.AgentAdminView{
		ID:           a.ID,
		Name:         a.Name,
		Provider:     a.Provider,
		BaseURL:      a.BaseURL,
		Model:        a.Model,
		SystemPrompt: a.SystemPrompt,
		Vision:       a.Vision,
		ToolsEnabled: a.ToolsEnabled,
		Enabled:      a.Enabled,
		APIKeySet:    strings.TrimSpace(a.APIKeyEnc) != "",
	}
}
