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

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler renders the global /inbox page and (later) handles the
// infinite-scroll fetch + sender-attach popover endpoints. Phase 1
// implements only the page shell.
type Handler struct {
	Repo          *Repo
	AuthRepo      *auth.Repo
	CommunityRepo *community.Repo
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

	page := webtempl.InboxPageData{
		Viewer:          viewerOf(id),
		Pills:           pills,
		ActiveCommunity: communityFilter,
		Rows:            toViewRows(views, pills),
		NextCursor:      encodeCursor(next),
	}
	_ = webtempl.InboxPage(page).Render(r.Context(), w)
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

func toViewRows(rows []QueuedEmailView, pills []webtempl.InboxPill) []webtempl.InboxRow {
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
			CommunityName:   pillByID[r.CommunityID].Name,
			CommunitySlug:   pillByID[r.CommunityID].Slug,
			FromAddr:        r.FromAddr,
			FromName:        r.FromName,
			Subject:         r.Subject,
			ReceivedAtUnix:  r.ReceivedAt.Unix(),
			AttachmentCount: len(atts),
			Attachments:     atts,
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
