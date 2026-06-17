package push

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// Handler exposes the /push/* HTTP endpoints and the per-community
// notification settings page.
type Handler struct {
	Repo      *Repo
	Sender    *Sender
	PublicKey string
	AuthSvc   *auth.Service
	AuthRepo  *auth.Repo
	Log       *slog.Logger
}

// Mount registers /push/config and the authenticated /push/* endpoints.
// The per-community notifications page is mounted by the community
// router so it picks up the community-context middleware.
func (h *Handler) Mount(r chi.Router) {
	// Public — service worker fetches the VAPID public key.
	r.Get("/push/config", h.GetConfig)

	// Authenticated.
	r.Group(func(g chi.Router) {
		g.Use(auth.RequireAuth)
		g.Post("/push/subscribe", h.PostSubscribe)
		g.Post("/push/unsubscribe", h.PostUnsubscribe)
	})
}

// MountPerCommunity registers the settings UI + settings POST under a
// per-community route group that already has the community-context
// middleware installed.
func (h *Handler) MountPerCommunity(r chi.Router) {
	r.Get("/notifications", h.GetSettingsPage)
	r.Post("/notifications/save", h.PostSettings)
}

// GetConfig — service worker hits this once on register.
func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"publicKey": h.PublicKey,
	})
}

type subscribeIn struct {
	CommunityID   string   `json:"community_id"`
	Endpoint      string   `json:"endpoint"`
	P256dh        string   `json:"p256dh"`
	AuthKey       string   `json:"auth_key"`
	UserAgent     string   `json:"user_agent"`
	Settings      Settings `json:"settings"`
	DigestMinutes int      `json:"digest_minutes"`
}

// PostSubscribe persists a browser PushSubscription for the current
// user + community. Body is JSON.
func (h *Handler) PostSubscribe(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in subscribeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	in.CommunityID = strings.TrimSpace(in.CommunityID)
	in.Endpoint = strings.TrimSpace(in.Endpoint)
	if in.CommunityID == "" || in.Endpoint == "" || in.P256dh == "" || in.AuthKey == "" {
		http.Error(w, "missing fields", http.StatusBadRequest)
		return
	}
	if in.Settings == nil {
		in.Settings = Settings{}
	}
	digest := in.DigestMinutes
	if digest < 0 {
		digest = 0
	}
	if digest > 1440 {
		digest = 1440
	}
	err := h.Repo.Upsert(r.Context(), Subscription{
		ID:            uuid.NewString(),
		UserID:        id.User.ID,
		CommunityID:   in.CommunityID,
		Endpoint:      in.Endpoint,
		P256dh:        in.P256dh,
		AuthKey:       in.AuthKey,
		UserAgent:     in.UserAgent,
		Settings:      in.Settings,
		DigestMinutes: digest,
	})
	if err != nil {
		h.Log.Error("push subscribe upsert", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type unsubscribeIn struct {
	Endpoint string `json:"endpoint"`
}

// PostUnsubscribe removes a single subscription by endpoint.
func (h *Handler) PostUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	var in unsubscribeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if err := h.Repo.DeleteByEndpoint(r.Context(), id.User.ID, in.Endpoint); err != nil {
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetSettingsPage renders the per-community settings UI.
func (h *Handler) GetSettingsPage(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return
	}
	settings, has, err := h.Repo.SettingsFor(r.Context(), id.User.ID, c.ID)
	if err != nil {
		h.Log.Error("push settings load", "err", err)
		http.Error(w, "load failed", http.StatusInternalServerError)
		return
	}
	// Pull digest_minutes from any existing subscription for this
	// (user, community) so the dropdown reflects saved state. UpdateSettings
	// writes it uniformly so picking the first row is correct.
	digestMin := 0
	if has {
		subs, sErr := h.Repo.SubsForUserCommunity(r.Context(), id.User.ID, c.ID)
		if sErr == nil && len(subs) > 0 {
			digestMin = subs[0].DigestMinutes
		}
	}
	v := webtempl.Viewer{
		IsAuthed:      true,
		DisplayName:   id.Membership.DisplayName,
		Role:          string(id.Membership.Role),
		CommunityName: c.Name,
		CommunitySlug: c.Slug,
	}
	page := webtempl.NotificationsPage(webtempl.NotificationsPageData{
		Viewer:      v,
		PublicKey:   h.PublicKey,
		CommunityID: c.ID,
		Has:         has,
		Settings:    toTempl(settings, digestMin),
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Render(r.Context(), w)
}

type saveSignals struct {
	Mention    bool `json:"notif_mention"`
	Report     bool `json:"notif_report"`
	ProjectNew bool `json:"notif_project_new"`
	IssueNew   bool `json:"notif_issue_new"`
	CommentNew bool `json:"notif_comment_new"`
	ThreadNew  bool `json:"notif_thread_new"`
	ChatNew    bool `json:"notif_chat_new"`
	// 0 = immediate. Bigger values bucket events into a digest sent
	// at most every N minutes. Clamped server-side to [0, 1440].
	DigestMinutes int `json:"notif_digest_minutes"`
}

func (h *Handler) PostSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "auth required", http.StatusUnauthorized)
		return
	}
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.Error(w, "no community", http.StatusInternalServerError)
		return
	}
	var in saveSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	s := Settings{
		"mention":     in.Mention,
		"report":      in.Report,
		"project_new": in.ProjectNew,
		"issue_new":   in.IssueNew,
		"comment_new": in.CommentNew,
		"thread_new":  in.ThreadNew,
		"chat_new":    in.ChatNew,
	}
	digest := in.DigestMinutes
	if digest < 0 {
		digest = 0
	}
	if digest > 1440 {
		digest = 1440
	}
	if err := h.Repo.UpdateSettings(r.Context(), id.User.ID, c.ID, s, digest); err != nil {
		h.Log.Error("push settings save", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.SuccessFragment("notif-status", "Saved."))
}

func toTempl(s Settings, digestMinutes int) webtempl.NotificationSettings {
	get := func(k string) bool {
		if v, ok := s[k]; ok {
			return v
		}
		return true // opt-in by default
	}
	return webtempl.NotificationSettings{
		Mention:       get("mention"),
		Report:        get("report"),
		ProjectNew:    get("project_new"),
		IssueNew:      get("issue_new"),
		CommentNew:    get("comment_new"),
		ThreadNew:     get("thread_new"),
		ChatNew:       get("chat_new"),
		DigestMinutes: digestMinutes,
	}
}
