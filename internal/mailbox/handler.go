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
	if communityFilter != "" && communityFilter != UnassignedCommunityID && !contains(adminCIDs, communityFilter) {
		// Don't leak the existence of a community the viewer is not admin in.
		http.NotFound(w, r)
		return
	}

	cursor, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		http.Error(w, "bad cursor", http.StatusBadRequest)
		return
	}

	attachOnly := r.URL.Query().Get("attach") == "1"
	views, next, err := h.Repo.QueueForViewer(r.Context(), QueueQuery{
		AdminCommunityIDs: adminCIDs,
		CommunityFilter:   communityFilter,
		HasAttachments:    attachOnly,
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

	projsByCommunity, err := h.loadProjectsForViews(r.Context(), adminCIDs)
	if err != nil {
		h.Log.Error("mailbox: load projects", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	page := webtempl.InboxPageData{
		Viewer:          viewerOf(id),
		Pills:           pills,
		ActiveCommunity: communityFilter,
		HasAttachOnly:   attachOnly,
		Rows:            toViewRows(views, pills, projsByCommunity),
		NextCursor:      encodeCursor(next),
	}
	_ = webtempl.InboxPage(page).Render(r.Context(), w)
}

// loadProjectsForViews fetches active project options for every admin
// community the viewer can route into, so unassigned rows can pick
// across communities. One query per community is fine at our row cap.
//
// Each community gets a leading sentinel option "Inbox (auto)" with id
// InboxProjectSentinel + ":" + cid. Picking it sends materialise into
// Service.ensureInboxProject which finds-or-creates a community-scoped
// "Inbox" project. Guarantees every dropdown has at least one option
// even when the community has zero real projects yet.
func (h *Handler) loadProjectsForViews(ctx context.Context, adminCIDs []string) (projectOptionsByCommunity, error) {
	if h.Svc == nil || h.Svc.Projs == nil {
		return projectOptionsByCommunity{}, nil
	}
	out := projectOptionsByCommunity{}
	for _, cid := range adminCIDs {
		rows, err := h.Svc.Projs.ListActiveForCommunity(ctx, cid)
		if err != nil {
			return nil, err
		}
		opts := make([]webtempl.InboxProjectOption, 0, len(rows)+1)
		opts = append(opts, webtempl.InboxProjectOption{
			ID:    InboxProjectSentinel + ":" + cid,
			Title: "Inbox (auto)",
		})
		for _, r := range rows {
			opts = append(opts, webtempl.InboxProjectOption{ID: r.ID, Title: r.Title})
		}
		out[cid] = opts
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
		// Move target groups: matched rows get one group (their own
		// community); unassigned rows get one group per admin community
		// the viewer can route into. The Move handler resolves the
		// destination community from the chosen project's community_id.
		var groups []webtempl.InboxProjectGroup
		if r.CommunityID != "" {
			groups = append(groups, webtempl.InboxProjectGroup{
				CommunityID:   r.CommunityID,
				CommunityName: pillByID[r.CommunityID].Name,
				Projects:      projects[r.CommunityID],
			})
		} else {
			for _, p := range pills {
				groups = append(groups, webtempl.InboxProjectGroup{
					CommunityID:   p.ID,
					CommunityName: p.Name,
					Projects:      projects[p.ID],
				})
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
			BodyText:        r.BodyText,
			ReceivedAtUnix:  r.ReceivedAt.Unix(),
			AttachmentCount: len(atts),
			Attachments:     atts,
			ProjectGroups:   groups,
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
	if communityFilter != "" && communityFilter != UnassignedCommunityID && !contains(adminCIDs, communityFilter) {
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

	projsByCommunity, err := h.loadProjectsForViews(r.Context(), adminCIDs)
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
	if communityFilter != "" && communityFilter != UnassignedCommunityID && !contains(adminCIDs, communityFilter) {
		http.NotFound(w, r)
		return
	}

	sse := render.NewSSE(w, r)

	// Per-community subscription. The Bus internals demand one ch per id,
	// so we multiplex inside this handler by spawning a tiny goroutine
	// that forwards every community channel into a shared `wake` chan.
	// Subscribe to the viewer's admin communities AND the unassigned
	// channel so newly-arriving unfiltered mail wakes the page too.
	wake := make(chan struct{}, 1)
	var unsubs []func()
	subscribeBus := func(cid string) {
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
	for _, cid := range adminCIDs {
		subscribeBus(cid)
	}
	subscribeBus(UnassignedCommunityID)
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

	// Throttle full-list patches so a long initial Gmail crawl (one
	// broadcast per ingested email) doesn't morph the DOM out from
	// under a user who clicked into a row to read it. Wake events are
	// coalesced: any wake within minPatchInterval increments the
	// "pending new" counter via PatchSignals (cheap, no DOM touch);
	// the next patch goes out once the throttle clears OR when the
	// user clicks the "X new — refresh" banner.
	const minPatchInterval = 15 * time.Second
	var (
		lastPatch    time.Time
		pendingDelta int
	)
	bumpPending := func() {
		pendingDelta++
		payload := []byte(fmt.Sprintf(`{"inbox_pending":%d}`, pendingDelta))
		_ = sse.PatchSignals(payload)
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
		projsByCommunity, err := h.loadProjectsForViews(r.Context(), adminCIDs)
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
	lastPatch = time.Now()
	_ = sse.PatchSignals([]byte(`{"inbox_pending":0}`))

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	throttleCheck := time.NewTicker(2 * time.Second)
	defer throttleCheck.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-wake:
			if time.Since(lastPatch) < minPatchInterval {
				bumpPending()
				continue
			}
			if err := patchFirstPage(); err == nil {
				lastPatch = time.Now()
				pendingDelta = 0
				_ = sse.PatchSignals([]byte(`{"inbox_pending":0}`))
			}
		case <-throttleCheck.C:
			// If patches piled up during throttle window, apply now.
			if pendingDelta > 0 && time.Since(lastPatch) >= minPatchInterval {
				if err := patchFirstPage(); err == nil {
					lastPatch = time.Now()
					pendingDelta = 0
					_ = sse.PatchSignals([]byte(`{"inbox_pending":0}`))
				}
			}
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

	backfilled, err := h.Repo.InsertFilter(r.Context(), Filter{
		ID:          uuid.NewString(),
		CommunityID: in.CommunityID,
		Kind:        kind,
		Pattern:     pattern,
		ToIssue:     in.ToIssue,
		CreatedBy:   id.User.ID,
	})
	if err != nil {
		h.Log.Error("mailbox: InsertFilter", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	h.Log.Info("mailbox: filter attached from inbox popover",
		"community", in.CommunityID, "kind", kind, "pattern", pattern, "backfilled", backfilled)
	// Backfill moved rows out of Unassigned + into the chosen community;
	// broadcast both so both views refresh.
	h.broadcast(r.Context(), in.CommunityID)
	h.broadcast(r.Context(), UnassignedCommunityID)

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

	// Authorisation: viewer must be admin-of-any-community when the
	// ingest is unassigned, otherwise admin in the ingest's community.
	// The chosen project's community is later checked inside Materialise
	// so the rest of the flow still validates cross-community moves.
	look, err := h.Repo.AttachmentByID(r.Context(), attID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	adminCIDs, err := h.AuthRepo.AdminCommunityIDs(r.Context(), id.User.ID)
	if err != nil || len(adminCIDs) == 0 {
		http.NotFound(w, r)
		return
	}
	if look.Ingest.CommunityID != "" && !contains(adminCIDs, look.Ingest.CommunityID) {
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

// searchSignals is the typeahead input bound by the inbox search box.
type searchSignals struct {
	Query string `json:"inbox_q"`
}

// PostSearch runs an FTS5 query against email_ingest_fts scoped to the
// viewer's admin community set + active community pill. Returns an SSE
// that replaces the rows fragment. Empty queries fall back to the
// recent list so clearing the box re-shows the page-1 view.
func (h *Handler) PostSearch(w http.ResponseWriter, r *http.Request) {
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
	var in searchSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	communityFilter := strings.TrimSpace(r.URL.Query().Get("community"))
	if communityFilter != "" && communityFilter != UnassignedCommunityID && !contains(adminCIDs, communityFilter) {
		http.NotFound(w, r)
		return
	}

	views, err := h.Repo.SearchQueueForViewer(r.Context(), QueueQuery{
		AdminCommunityIDs: adminCIDs,
		CommunityFilter:   communityFilter,
		Limit:             100,
	}, in.Query)
	if err != nil {
		h.Log.Error("mailbox: search", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	pills, err := h.loadCommunityPills(r.Context(), adminCIDs)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	projs, err := h.loadProjectsForViews(r.Context(), adminCIDs)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}

	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(
		webtempl.InboxRowList(toViewRows(views, pills, projs)),
		datastar.WithSelector("#inbox-rows"),
		datastar.WithModeOuter(),
	)
	// While in search mode, hide the infinite-scroll sentinel so the
	// user doesn't paginate through the full feed below their hits.
	cursor := ""
	if strings.TrimSpace(in.Query) == "" {
		// Empty query reverts to "show first page + show sentinel".
		// Cursor for "next page" comes from QueueForViewer; re-run it
		// here to get the right next pointer.
		_, next, _ := h.Repo.QueueForViewer(r.Context(), QueueQuery{
			AdminCommunityIDs: adminCIDs,
			CommunityFilter:   communityFilter,
			Limit:             100,
		})
		cursor = encodeCursor(next)
	}
	_ = sse.PatchElementTempl(
		webtempl.InboxMore(cursor, communityFilter),
		datastar.WithSelector("#inbox-more"),
		datastar.WithModeOuter(),
	)
}

// GetCommunityFilters renders the per-community admin CRUD page that
// lists every filter targeting this community + offers a new-filter
// form. Mounted under /c/{slug}/admin/mail-filters; the route guard
// already enforces RequireRole(Admin).
func (h *Handler) GetCommunityFilters(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	filters, err := h.Repo.ListFiltersForCommunity(r.Context(), cm.ID)
	if err != nil {
		h.Log.Error("mailbox: list filters", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	page := webtempl.MailFiltersPageData{
		Viewer:        h.communityViewer(id, cm.Slug, cm.Name),
		CommunityID:   cm.ID,
		CommunitySlug: cm.Slug,
		CommunityName: cm.Name,
		Rows:          toFilterViewRows(filters),
	}
	_ = webtempl.MailFiltersPage(page).Render(r.Context(), w)
}

// communityFilterSignals carries the new-filter form payload.
type communityFilterSignals struct {
	Kind    string `json:"mf_kind"`
	Pattern string `json:"mf_pattern"`
	ToIssue bool   `json:"mf_to_issue"`
}

// PostCommunityFilterCreate handles the admin CRUD page's "Save
// filter" submit. Shares Repo.InsertFilter with the popover path.
func (h *Handler) PostCommunityFilterCreate(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in communityFilterSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals", http.StatusBadRequest)
		return
	}
	kind := FilterKind(in.Kind)
	if kind != FilterKindAddress && kind != FilterKindDomain {
		http.Error(w, "bad kind", http.StatusBadRequest)
		return
	}
	pattern := normaliseFilterPattern(kind, in.Pattern)
	if pattern == "" {
		http.Error(w, "bad pattern", http.StatusBadRequest)
		return
	}
	backfilled, err := h.Repo.InsertFilter(r.Context(), Filter{
		ID:          uuid.NewString(),
		CommunityID: cm.ID,
		Kind:        kind,
		Pattern:     pattern,
		ToIssue:     in.ToIssue,
		CreatedBy:   id.User.ID,
	})
	if err != nil {
		h.Log.Error("mailbox: InsertFilter", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	h.Log.Info("mailbox: filter created from admin page",
		"community", cm.ID, "kind", kind, "pattern", pattern, "backfilled", backfilled)
	h.broadcast(r.Context(), cm.ID)
	h.broadcast(r.Context(), UnassignedCommunityID)

	filters, _ := h.Repo.ListFiltersForCommunity(r.Context(), cm.ID)
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(
		webtempl.MailFiltersTable(toFilterViewRows(filters), cm.Slug),
		datastar.WithSelector("#mail-filters-table"),
		datastar.WithModeOuter(),
	)
	_ = sse.PatchSignals([]byte(`{"mf_pattern":"","mf_to_issue":false}`))
}

// PostCommunityFilterDelete removes one filter row.
func (h *Handler) PostCommunityFilterDelete(w http.ResponseWriter, r *http.Request) {
	cm, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	fid := chi.URLParam(r, "id")
	if fid == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := h.Repo.DeleteFilter(r.Context(), fid, cm.ID); err != nil {
		h.Log.Error("mailbox: DeleteFilter", "err", err)
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	h.broadcast(r.Context(), cm.ID)

	filters, _ := h.Repo.ListFiltersForCommunity(r.Context(), cm.ID)
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(
		webtempl.MailFiltersTable(toFilterViewRows(filters), cm.Slug),
		datastar.WithSelector("#mail-filters-table"),
		datastar.WithModeOuter(),
	)
}

func (h *Handler) communityViewer(id auth.Identity, slug, name string) webtempl.Viewer {
	return webtempl.Viewer{
		IsAuthed:              true,
		DisplayName:           id.Membership.DisplayName,
		Role:                  string(id.Membership.Role),
		CommunityName:         name,
		CommunitySlug:         slug,
		IsAdminOfAnyCommunity: true,
	}
}

func toFilterViewRows(rows []Filter) []webtempl.MailFilterRow {
	out := make([]webtempl.MailFilterRow, len(rows))
	for i, f := range rows {
		out[i] = webtempl.MailFilterRow{
			ID:        f.ID,
			Kind:      string(f.Kind),
			Pattern:   f.Pattern,
			ToIssue:   f.ToIssue,
			CreatedAt: f.CreatedAt.Unix(),
		}
	}
	return out
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
