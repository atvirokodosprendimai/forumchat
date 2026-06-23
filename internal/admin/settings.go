package admin

import (
	"net/http"
	"strings"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/netguard"
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
	RAGEnabled       bool   `json:"set_rag_enabled"`
	RAGEmbedBaseURL  string `json:"set_rag_embed_base_url"`
	RAGEmbedModel    string `json:"set_rag_embed_model"`
	RAGEmbedDim      int    `json:"set_rag_embed_dim"`
	RAGQdrantURL     string `json:"set_rag_qdrant_url"`
	RAGQdrantAPIKey  string `json:"set_rag_qdrant_api_key"` // write-only: blank = keep existing
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
	// SSRF guard: tenant-supplied outbound URLs must not target internal hosts
	// (the platform dials them). Self-host is exempt — it legitimately uses
	// localhost daemons.
	if h.Cfg.SAAS {
		for _, u := range []string{in.TranslateBaseURL, in.RAGEmbedBaseURL, in.RAGQdrantURL} {
			if blocked, reason := netguard.BlockedURL(u); blocked {
				sse := render.NewSSE(w, r)
				_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-settings-error", "Rejected URL — "+reason))
				return
			}
		}
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
	rag := in.RAGEnabled
	s.RAGEnabled = &rag
	s.RAGEmbedBaseURL = in.RAGEmbedBaseURL
	s.RAGEmbedModel = in.RAGEmbedModel
	s.RAGEmbedDim = in.RAGEmbedDim
	s.RAGQdrantURL = in.RAGQdrantURL
	// Write-only secret: only overwrite when the owner typed a new key, so a
	// blank field on save keeps the stored key rather than wiping it.
	if strings.TrimSpace(in.RAGQdrantAPIKey) != "" {
		s.RAGQdrantAPIKey = in.RAGQdrantAPIKey
	}
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
	rag := community.ResolveRAG(s, h.Cfg)
	d := webtempl.OwnerSettingsData{
		Viewer:           h.viewer(r),
		CommunitySlug:    c.Slug,
		CommunityName:    c.Name,
		AIEnabled:        community.EffectiveAIEnabled(s, h.Cfg),
		JoinPolicy:       community.JoinPolicy(s, h.Cfg),
		TranslateEnabled: tr.Enabled,
		TranslateBaseURL: tr.BaseURL,
		TranslateModel:   tr.Model,
		RAGEnabled:       rag.Enabled,
		RAGEmbedBaseURL:  rag.EmbedBaseURL,
		RAGEmbedModel:    rag.EmbedModel,
		RAGEmbedDim:      rag.EmbedDim,
		RAGQdrantURL:     rag.QdrantURL,
		RAGHasQdrantKey:  s.RAGQdrantAPIKey != "",
		Saved:            saved,
	}
	return d
}
