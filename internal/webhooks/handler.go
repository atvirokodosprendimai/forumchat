package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
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
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
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

	// Uploads, if set, lets the inbound generic webhook accept media via
	// multipart/form-data (a text field + file parts) — the bytes are stored
	// and posted as chat attachments. nil disables inbound media (text only).
	Uploads *uploads.Store

	// Forum-routing seams (optional). When an inbound generic message carries a
	// thread_key, it is mirrored into the forum instead of the chat channel:
	// OpenForumThread opens a thread the first time a key is seen, AddForumPost
	// appends to it thereafter, and NotifyForumThread wakes open forum viewers.
	// All three are wired in main.go to forum (closures, no import). nil
	// disables forum routing — a thread_key then falls back to the chat path.
	OpenForumThread   func(ctx context.Context, communityID, author, subject, markdown string) (threadID string, err error)
	AddForumPost      func(ctx context.Context, threadID, author, avatar, markdown string) (postID string, err error)
	NotifyForumThread func(communityID, threadID string)
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

	// Media ingest: a generic webhook may POST multipart/form-data (a `text`
	// field plus one or more `file` parts) to post images/files into the
	// channel. Handled here (the adapter only sees JSON bytes).
	if wh.Provider == "generic" && h.Uploads != nil &&
		strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		h.postInboundMultipart(w, r, wh)
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

	// A thread_key routes the message into the forum (Matrix-thread sync) when
	// the forum seams are wired; otherwise it falls through to the chat path.
	if rendered.ThreadKey != "" && h.OpenForumThread != nil {
		h.postInboundForum(w, r, wh, rendered)
		return
	}

	// Inline chat threading: reply_to_key nests this message under a prior
	// bridged chat message; message_key records this one so a later reply can
	// target it. The far-side speaker's name (rendered.Author) rides the bot
	// identity here, like the forum path, so a thread reads alice/bob/carol
	// rather than the webhook's own label.
	parentID := h.resolveReplyParent(r.Context(), wh.ID, rendered.ReplyToKey)
	msg, err := h.Chat.PostBot(r.Context(), wh.CommunityID, wh.ChannelID, inboundAuthor(wh, rendered), wh.AvatarURL, rendered.Markdown, parentID)
	if err != nil {
		h.Log.Error("webhooks: PostBot", "err", err)
		_ = h.Repo.Stamp(r.Context(), wh.ID, "error")
		http.Error(w, "post failed", http.StatusInternalServerError)
		return
	}
	h.linkInboundMessage(r.Context(), wh.ID, rendered.MessageKey, msg.ID)
	h.fanout(wh.CommunityID, wh.ChannelID)
	_ = h.Repo.Stamp(r.Context(), wh.ID, "ok")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// postInboundForum mirrors an inbound message into the forum: the first message
// for a given thread_key opens a thread; later messages with the same key
// append posts. Forum content is bot-authored (the far-side human's name rides
// the bot identity), so it never relays back out — the inbound echo guard. The
// response returns the forum thread/post ids so a stateful bridge can record
// the reverse (forumchat -> external) mapping.
// postInboundMultipart ingests a multipart/form-data inbound webhook: an
// optional `text`/`content` field plus one or more `file` (or `files`) parts.
// Each file is stored as an upload owned by a synthetic "webhook:<id>" owner
// and the message is posted into the channel with those attachments. Enables an
// external bridge (e.g. Matrix → forumchat) to deliver images.
func (h *Handler) postInboundMultipart(w http.ResponseWriter, r *http.Request, wh Webhook) {
	// Size to the upload cap (media), not the small JSON MaxBytes.
	max := h.Uploads.MaxSize
	if max <= 0 {
		max = 1 << 20
	}
	if err := r.ParseMultipartForm(max + 1024); err != nil {
		http.Error(w, "bad multipart: "+err.Error(), http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if text == "" {
		text = strings.TrimSpace(r.FormValue("content"))
	}
	// Inline chat threading also works for media replies (same envelope as the
	// JSON path, carried as form fields here).
	replyToKey := strings.TrimSpace(r.FormValue("reply_to_key"))
	messageKey := strings.TrimSpace(r.FormValue("message_key"))
	botName := wh.Name
	if author := strings.TrimSpace(r.FormValue("author")); author != "" {
		botName = author
	}

	// Uploads are FK'd to a real user; attribute webhook media to the webhook's
	// creator. Without one we can't store files (text still posts).
	owner := wh.CreatedBy
	var ids []string
	if r.MultipartForm != nil {
		headers := append([]*multipart.FileHeader{}, r.MultipartForm.File["file"]...)
		headers = append(headers, r.MultipartForm.File["files"]...)
		if len(headers) > 0 && owner == "" {
			h.Log.Warn("webhooks: inbound media skipped — webhook has no creator", "webhook", wh.ID)
		}
		for _, fh := range headers {
			if owner == "" {
				break
			}
			id, err := h.saveMultipartFile(r.Context(), owner, wh.CommunityID, fh)
			if err != nil {
				h.Log.Warn("webhooks: inbound media save", "err", err)
				continue
			}
			ids = append(ids, id)
		}
	}

	if text == "" && len(ids) == 0 {
		_ = h.Repo.Stamp(r.Context(), wh.ID, "skip")
		w.WriteHeader(http.StatusOK)
		return
	}

	parentID := h.resolveReplyParent(r.Context(), wh.ID, replyToKey)
	msg, err := h.Chat.PostBotWithAttachments(r.Context(), wh.CommunityID, wh.ChannelID, botName, wh.AvatarURL, text, ids, parentID)
	if err != nil {
		h.Log.Error("webhooks: PostBotWithAttachments", "err", err)
		_ = h.Repo.Stamp(r.Context(), wh.ID, "error")
		http.Error(w, "post failed", http.StatusInternalServerError)
		return
	}
	h.linkInboundMessage(r.Context(), wh.ID, messageKey, msg.ID)
	h.fanout(wh.CommunityID, wh.ChannelID)
	_ = h.Repo.Stamp(r.Context(), wh.ID, "ok")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// saveMultipartFile stores one multipart file part as an upload, sniffing the
// MIME from the part's declared Content-Type and leading bytes.
func (h *Handler) saveMultipartFile(ctx context.Context, owner, communityID string, fh *multipart.FileHeader) (string, error) {
	f, err := fh.Open()
	if err != nil {
		return "", err
	}
	defer f.Close()
	sniff := make([]byte, 512)
	n, _ := f.Read(sniff)
	sniff = sniff[:n]
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}
	mimeType := uploads.MIMEFromHeader(fh.Header.Get("Content-Type"), sniff)
	u, err := h.Uploads.Save(ctx, owner, communityID, mimeType, fh.Filename, f)
	if err != nil {
		return "", err
	}
	return u.ID, nil
}

func (h *Handler) postInboundForum(w http.ResponseWriter, r *http.Request, wh Webhook, rendered Rendered) {
	author := rendered.Author
	if author == "" {
		author = wh.Name
	}

	threadID, err := h.Repo.ThreadLink(r.Context(), wh.ID, rendered.ThreadKey)
	var postID string
	switch {
	case errors.Is(err, ErrNotFound):
		threadID, err = h.OpenForumThread(r.Context(), wh.CommunityID, author, rendered.Subject, rendered.Markdown)
		if err != nil {
			h.Log.Error("webhooks: open forum thread", "err", err)
			_ = h.Repo.Stamp(r.Context(), wh.ID, "error")
			http.Error(w, "post failed", http.StatusInternalServerError)
			return
		}
		if err := h.Repo.LinkThread(r.Context(), wh.ID, rendered.ThreadKey, threadID); err != nil {
			h.Log.Warn("webhooks: link forum thread", "err", err)
		}
	case err != nil:
		h.Log.Error("webhooks: thread link lookup", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	default:
		if h.AddForumPost == nil {
			http.Error(w, "forum routing unavailable", http.StatusServiceUnavailable)
			return
		}
		postID, err = h.AddForumPost(r.Context(), threadID, author, wh.AvatarURL, rendered.Markdown)
		if err != nil {
			h.Log.Error("webhooks: add forum post", "err", err)
			_ = h.Repo.Stamp(r.Context(), wh.ID, "error")
			http.Error(w, "post failed", http.StatusInternalServerError)
			return
		}
	}

	if h.NotifyForumThread != nil {
		h.NotifyForumThread(wh.CommunityID, threadID)
	}
	_ = h.Repo.Stamp(r.Context(), wh.ID, "ok")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"thread_id": threadID, "post_id": postID})
}

// inboundAuthor picks the display name for a bridged chat message: the far-side
// speaker's name when the payload carries one, else the webhook's own name —
// the same attribution the forum path uses, so flat and threaded bridged
// messages name the actual person rather than the webhook label.
func inboundAuthor(wh Webhook, rendered Rendered) string {
	if rendered.Author != "" {
		return rendered.Author
	}
	return wh.Name
}

// resolveReplyParent maps an inbound reply_to_key to a prior chat message id for
// this webhook, or "" when the key is empty or unknown (post flat). A lookup
// error other than "not found" is logged but still degrades to a flat post —
// threading is best-effort, never a delivery failure.
func (h *Handler) resolveReplyParent(ctx context.Context, webhookID, replyToKey string) string {
	if replyToKey == "" {
		return ""
	}
	id, err := h.Repo.MessageLink(ctx, webhookID, replyToKey)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			h.Log.Warn("webhooks: message link lookup", "err", err)
		}
		return ""
	}
	return id
}

// linkInboundMessage records message_key -> chat message id so a later inbound
// reply_to_key can nest under it. No-op when the key is empty.
func (h *Handler) linkInboundMessage(ctx context.Context, webhookID, messageKey, msgID string) {
	if messageKey == "" {
		return
	}
	if err := h.Repo.LinkMessage(ctx, webhookID, messageKey, msgID); err != nil {
		h.Log.Warn("webhooks: link inbound message", "err", err)
	}
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
