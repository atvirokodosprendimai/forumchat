package mailbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"

	natsgo "github.com/nats-io/nats.go"
)

// Handler renders the global /inbox page, the infinite-scroll fetch,
// the per-community SSE stream, the click-sender-attach popover, and
// the lazy attachment materialise endpoint.
type Handler struct {
	Repo          *Repo
	AuthRepo      *auth.Repo
	CommunityRepo *community.Repo
	Svc           *Service // optional — required for PostMoveAttachment
	Bus           *Bus
	NATS          *natsgo.Conn // optional — nil disables cross-process fan-out
	Log           *slog.Logger
}

// GetGlobalInbox renders /inbox for an admin-of-any-community viewer.
// Non-admin and unauthenticated visitors get a 404 (anti-enumeration —
// the page must appear not-to-exist to anyone unauthorised, see the
// spec's anti-enumeration block).
func (h *Handler) GetGlobalInbox(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}

	adminCIDs, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil {
		h.Log.Error("mailbox: load admin community ids", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if len(adminCIDs) == 0 {
		http.NotFound(w, r)
		return
	}

	communityFilter := strings.TrimSpace(r.URL.Query().Get("community"))
	if communityFilter != "" && !contains(adminCIDs, communityFilter) {
		// Don't leak the existence of a community the viewer is not admin in.
		http.NotFound(w, r)
		return
	}

	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}

	views, next, err := h.Repo.QueueForViewer(r.Context(), QueueQuery{
		AdminCommunityIDs: adminCIDs,
		CommunityFilter:   communityFilter,
		Cursor:            cursor,
		Limit:             100,
	})
	if err != nil {
		h.Log.Error("mailbox: queue load", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	pills, err := h.loadCommunityPills(r.Context(), adminCIDs)
	if err != nil {
		h.Log.Error("mailbox: load pills", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	projsByCommunity, err := h.loadProjectsForViews(r.Context(), views)
	if err != nil {
		h.Log.Error("mailbox: load projects", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	page := webtempl.InboxPageData{
		Viewer:          viewerOf(id),
		Pills:           pills,
		ActiveCommunity: communityFilter,
		Rows:            toViewRows(views, pills, projsByCommunity),
		NextCursor:      encodeCursor(next),
	}
	_ = webtempl.InboxPage(page).Render(r.Context(), w)
}

// loadProjectsForViews fetches active project options for every
// community represented in the page. One query per community is fine
// at our row cap.
func (h *Handler) loadProjectsForViews(ctx context.Context, views []QueuedEmailView) (projectOptionsByCommunity, error) {
	if h.Svc == nil || h.Svc.Projs == nil {
		return projectOptionsByCommunity{}, nil
	}
	seen := map[string]struct{}{}
	out := projectOptionsByCommunity{}
	for _, v := range views {
		if _, dup := seen[v.CommunityID]; dup {
			continue
		}
		seen[v.CommunityID] = struct{}{}
		rows, err := h.Svc.Projs.ListActiveForCommunity(ctx, v.CommunityID)
		if err != nil {
			return nil, err
		}
		opts := make([]webtempl.InboxProjectOption, len(rows))
		for i, r := range rows {
			opts[i] = webtempl.InboxProjectOption{ID: r.ID, Title: r.Title}
		}
		out[v.CommunityID] = opts
	}
	return out, nil
}

// viewerOf assembles the Viewer the layout needs. The mailbox-link
// gate is driven by webtempl.MailboxEnabled + Viewer.IsAdminOfAnyCommunity,
// which is true here by construction (the route would have 404'd above).
func viewerOf(id auth.Identity) webtempl.Viewer {
	return webtempl.Viewer{
		IsAuthed:               true,
		DisplayName:            id.Membership.DisplayName,
		Role:                   string(id.Membership.Role),
		CommunityName:          "",
		CommunitySlug:          "",
		IsAdminOfAnyCommunity:  true,
	}
}

// loadProjectOptionsByCommunity returns active project options grouped
// by community id, used by the per-attachment Move dropdown. Phase 6
// queries this once per page render which is fine for the cap of 100
// rows; if it ever bites perf the answer is a single JOIN inside the
// queue query.
type projectOptionsByCommunity = map[string][]webtempl.InboxProjectOption

// loadCommunityPills resolves community IDs the viewer is admin in to
// their (id, slug, name) tuples so the UI can render labelled pills.
func (h *Handler) loadCommunityPills(ctx context.Context, ids []string) ([]webtempl.InboxPill, error) {
	out := make([]webtempl.InboxPill, 0, len(ids))
	for _, cid := range ids {
		c, err := h.CommunityRepo.ByID(ctx, cid)
		if err != nil {
			return nil, fmt.Errorf("pill lookup %s: %w", cid, err)
		}
		out = append(out, webtempl.InboxPill{ID: c.ID, Slug: c.Slug, Name: c.Name})
	}
	return out, nil
}

func toViewRows(rows []QueuedEmailView, pills []webtempl.InboxPill, projects projectOptionsByCommunity) []webtempl.InboxRow {
	pillByID := make(map[string]webtempl.InboxPill, len(pills))
	for _, p := range pills {
		pillByID[p.ID] = p
	}
	out := make([]webtempl.InboxRow, len(rows))
	for i, r := range rows {
		atts := make([]webtempl.InboxAttachment, len(r.Attachments))
		for j, a := range r.Attachments {
			atts[j] = webtempl.InboxAttachment{
				ID:             a.ID,
				Filename:       a.Filename,
				MIME:           a.MIME,
				SizeBytes:      a.SizeBytes,
				IsMaterialised: a.IsMaterialised,
			}
		}
		out[i] = webtempl.InboxRow{
			ID:              r.ID,
			CommunityID:     r.CommunityID,
			CommunityName:   pillByID[r.CommunityID].Name,
			CommunitySlug:   pillByID[r.CommunityID].Slug,
			FromAddr:        r.FromAddr,
			FromName:        r.FromName,
			Subject:         r.Subject,
			ReceivedAtUnix:  r.ReceivedAt.Unix(),
			AttachmentCount: len(atts),
			Attachments:     atts,
			Projects:        projects[r.CommunityID],
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// encodeCursor produces the opaque base64-url token consumed by the next
// page request. Empty when there is no more.
func encodeCursor(c *QueueCursor) string {
	if c == nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.ReceivedAtUnixMS, 10) + ":" + c.ID))
}

// decodeCursor parses the token. Empty input → nil cursor (first page).
func decodeCursor(s string) (*QueueCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("bad cursor encoding")
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return nil, errors.New("bad cursor shape")
	}
	ms, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, errors.New("bad cursor ms")
	}
	return &QueueCursor{ReceivedAtUnixMS: ms, ID: parts[1]}, nil
}

// GetMore returns the next page of inbox rows via SSE. Datastar's
// scrollend handler hits this when the user reaches the sentinel. The
// response appends rows to `#inbox-rows` and replaces `#inbox-more`
// with the next sentinel (or empty when exhausted).
func (h *Handler) GetMore(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	adminCIDs, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil || len(adminCIDs) == 0 {
		http.NotFound(w, r)
		return
	}
	communityFilter := strings.TrimSpace(r.URL.Query().Get("community"))
	if communityFilter != "" && !contains(adminCIDs, communityFilter) {
		http.NotFound(w, r)
		return
	}
	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}
	views, next, err := h.Repo.QueueForViewer(r.Context(), QueueQuery{
		AdminCommunityIDs: adminCIDs,
		CommunityFilter:   communityFilter,
		Cursor:            cursor,
		Limit:             100,
	})
	if err != nil {
		h.Log.Error("mailbox: GetMore queue", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	pills, err := h.loadCommunityPills(r.Context(), adminCIDs)
	if err != nil {
		h.Log.Error("mailbox: GetMore pills", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	projsByCommunity, err := h.loadProjectsForViews(r.Context(), views)
	if err != nil {
		h.Log.Error("mailbox: GetMore projects", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	sse := render.NewSSE(w, r)
	rows := toViewRows(views, pills, projsByCommunity)
	_ = sse.PatchElementTempl(
		webtempl.InboxRowList(rows),
		datastar.WithSelector("#inbox-rows"),
		datastar.WithModeAppend(),
	)
	_ = sse.PatchElementTempl(
		webtempl.InboxMore(encodeCursor(next), communityFilter),
		datastar.WithSelector("#inbox-more"),
		datastar.WithModeOuter(),
	)
}

// GetStream is the long-lived SSE the inbox page opens once. When any
// of the viewer's admin communities publishes a mailbox event (the
// poll worker landed a new ingest row, or someone attached a sender),
// the stream re-renders the first page so the user sees fresh mail
// without manual refresh.
func (h *Handler) GetStream(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	adminCIDs, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil || len(adminCIDs) == 0 {
		http.NotFound(w, r)
		return
	}
	communityFilter := strings.TrimSpace(r.URL.Query().Get("community"))
	if communityFilter != "" && !contains(adminCIDs, communityFilter) {
		http.NotFound(w, r)
		return
	}

	sse := render.NewSSE(w, r)

	// Per-community subscription. The Bus internals demand one ch per id,
	// so we multiplex inside this handler by spawning a tiny goroutine
	// that forwards every community channel into a shared `wake` chan.
	wake := make(chan struct{}, 1)
	var unsubs []func()
	for _, cid := range adminCIDs {
		ch, unsub := h.Bus.Subscribe(cid)
		unsubs = append(unsubs, unsub)
		go func(in <-chan struct{}) {
			for range in {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}(ch)
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	// Optional cross-process bus via NATS.
	var natsCh chan *natsgo.Msg
	if h.NATS != nil && h.NATS.IsConnected() {
		natsCh = make(chan *natsgo.Msg, 16)
		for _, cid := range adminCIDs {
			sub, err := h.NATS.ChanSubscribe(natsx.MailboxSubject(cid), natsCh)
			if err == nil {
				defer sub.Unsubscribe() //nolint:errcheck
			}
		}
		go func() {
			for range natsCh {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}()
	}

	pills, err := h.loadCommunityPills(r.Context(), adminCIDs)
	if err != nil {
		return
	}

	// Initial render so the freshly-opened stream replaces any stale list.
	patchFirstPage := func() error {
		views, next, err := h.Repo.QueueForViewer(r.Context(), QueueQuery{
			AdminCommunityIDs: adminCIDs,
			CommunityFilter:   communityFilter,
			Limit:             100,
		})
		if err != nil {
			return err
		}
		projsByCommunity, err := h.loadProjectsForViews(r.Context(), views)
		if err != nil {
			return err
		}
		if err := sse.PatchElementTempl(
			webtempl.InboxRowList(toViewRows(views, pills, projsByCommunity)),
			datastar.WithSelector("#inbox-rows"),
			datastar.WithModeOuter(),
		); err != nil {
			return err
		}
		return sse.PatchElementTempl(
			webtempl.InboxMore(encodeCursor(next), communityFilter),
			datastar.WithSelector("#inbox-more"),
			datastar.WithModeOuter(),
		)
	}
	_ = patchFirstPage()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			_ = patchFirstPage()
		case <-keepalive.C:
			// Heartbeat — keeps load balancers from closing the
			// idle connection. Empty PatchSignals is a no-op on the
			// client but counts as live data on the wire.
			_ = sse.PatchSignals([]byte(`{}`))
		}
	}
}

// attachSenderSignals carries the popover payload. Fields are bound by
// the inbox dialog template.
type attachSenderSignals struct {
	Addr        string `json:"attach_addr"`
	Kind        string `json:"attach_kind"` // "address" | "domain"
	CommunityID string `json:"attach_community"`
	ToIssue     bool   `json:"attach_to_issue"`
}

// PostAttachSender creates a community_mail_filter row from the inbox
// popover. The viewer MUST be admin in the chosen community.
func (h *Handler) PostAttachSender(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	adminCIDs, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil || len(adminCIDs) == 0 {
		http.NotFound(w, r)
		return
	}
	var in attachSenderSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	if !contains(adminCIDs, in.CommunityID) {
		http.NotFound(w, r)
		return
	}
	kind := FilterKind(in.Kind)
	if kind != FilterKindAddress && kind != FilterKindDomain {
		http.Error(w, "bad kind", http.StatusBadRequest)
		return
	}
	pattern := normaliseFilterPattern(kind, in.Addr)
	if pattern == "" {
		http.Error(w, "bad pattern", http.StatusBadRequest)
		return
	}

	if err := h.Repo.InsertFilter(r.Context(), Filter{
		ID:          uuid.NewString(),
		CommunityID: in.CommunityID,
		Kind:        kind,
		Pattern:     pattern,
		ToIssue:     in.ToIssue,
		CreatedBy:   id.User.ID,
	}); err != nil {
		h.Log.Error("mailbox: InsertFilter", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	h.broadcast(r.Context(), in.CommunityID)

	sse := render.NewSSE(w, r)
	_ = sse.PatchSignals([]byte(`{"attach_open":false,"attach_addr":"","attach_kind":"address","attach_community":"","attach_to_issue":false}`))
}

// moveSignals captures the per-attachment Move form payload. Field
// names match the JSON keys the inbox template fetch() body sends.
type moveSignals struct {
	ProjectID string `json:"project_id"`
	Category  string `json:"category"`
}

// PostMoveAttachment lazily fetches the chosen attachment's bytes from
// IMAP and pipes them through projects.Service.AddAttachment so the
// file lands as a project_attachments row. The viewer must be admin in
// the ingest's community AND the chosen project must belong to that
// community (guard inside Svc.Materialise). Returns 204 on success;
// the inbox SSE morph picks up the change via Bus.Broadcast.
func (h *Handler) PostMoveAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	if h.Svc == nil {
		http.Error(w, "mailbox service not wired", http.StatusServiceUnavailable)
		return
	}
	attID := chi.URLParam(r, "id")
	if attID == "" {
		http.Error(w, "missing attachment id", http.StatusBadRequest)
		return
	}
	var in moveSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if in.ProjectID == "" {
		http.Error(w, "project required", http.StatusBadRequest)
		return
	}

	// Authorisation: viewer must be admin in the attachment's community.
	look, err := h.Repo.AttachmentByID(r.Context(), attID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	adminCIDs, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil || !contains(adminCIDs, look.Ingest.CommunityID) {
		http.NotFound(w, r)
		return
	}

	res, err := h.Svc.Materialise(r.Context(), MaterialiseInput{
		AttachmentID: attID,
		ProjectID:    in.ProjectID,
		Category:     strings.TrimSpace(in.Category),
		MoverID:      id.User.ID,
	})
	if err != nil {
		h.Log.Error("mailbox: Materialise", "err", err)
		http.Error(w, "materialise failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.broadcast(r.Context(), res.CommunityID)
	w.WriteHeader(http.StatusNoContent)
}

// broadcast pings the in-process Bus and (if connected) publishes the
// community id over NATS. Cross-process subscribers wake; same-process
// SSE loops also wake.
func (h *Handler) broadcast(_ context.Context, communityID string) {
	if h.Bus != nil {
		h.Bus.Broadcast(communityID)
	}
	if h.NATS != nil && h.NATS.IsConnected() {
		_ = h.NATS.Publish(natsx.MailboxSubject(communityID), []byte(communityID))
	}
}
