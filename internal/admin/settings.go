package admin

import (
	"net/http"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

// ownerSettingsSignals is the datastar bag for the owner Settings page. Only the
// fields the page controls are read; RAG + storage settings (heavier backends)
// are preserved across a save by loading the current row first.
type ownerSettingsSignals struct {
	AIEnabled        bool   `json:"set_ai_enabled"`
	JoinPolicy       string `json:"set_join_policy"`
	TranslateEnabled bool   `json:"set_translate_enabled"`
	TranslateBaseURL string `json:"set_translate_base_url"`
	TranslateModel   string `json:"set_translate_model"`
}

// GetSettings renders the owner-only tenant Settings page (SaaS). Owner-gated by
// the route; super-admins pass via the synthetic owner membership.
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	s, err := h.Communities.Settings(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = webtempl.OwnerSettingsPage(h.settingsData(r, c, s, false)).Render(r.Context(), w)
}

// PostSettings persists the AI / join-policy / translate fields, preserving the
// RAG + storage fields by overlaying onto the current row.
func (h *Handler) PostSettings(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in ownerSettingsSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	s, err := h.Communities.Settings(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ai := in.AIEnabled
	s.AIEnabled = &ai
	if in.JoinPolicy == "open" || in.JoinPolicy == "request" {
		s.JoinPolicy = in.JoinPolicy
	}
	tr := in.TranslateEnabled
	s.TranslateEnabled = &tr
	s.TranslateBaseURL = in.TranslateBaseURL
	s.TranslateModel = in.TranslateModel
	if err := h.Communities.SaveSettings(r.Context(), s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.OwnerSettingsForm(h.settingsData(r, c, s, true)))
}

// settingsData maps the persisted Settings onto the view model, resolving the
// effective values against env so unset fields display the platform default.
func (h *Handler) settingsData(r *http.Request, c community.Community, s community.Settings, saved bool) webtempl.OwnerSettingsData {
	tr := community.ResolveTranslate(s, h.Cfg)
	d := webtempl.OwnerSettingsData{
		Viewer:           h.viewer(r),
		CommunitySlug:    c.Slug,
		CommunityName:    c.Name,
		AIEnabled:        community.EffectiveAIEnabled(s, h.Cfg),
		JoinPolicy:       community.JoinPolicy(s, h.Cfg),
		TranslateEnabled: tr.Enabled,
		TranslateBaseURL: tr.BaseURL,
		TranslateModel:   tr.Model,
		Saved:            saved,
	}
	return d
}
