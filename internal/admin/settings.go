package admin

import (
	"context"
	"net/http"
	"strings"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/netguard"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
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
	// Capture the embedding identity BEFORE overlaying — a model/dim change
	// invalidates the Qdrant collection (sized to the old dim) and must trigger
	// a reindex, or the worker stalls on a dimension mismatch.
	oldRAG := community.ResolveRAG(s, h.Cfg)
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
	if in.RAGEmbedDim < 0 {
		in.RAGEmbedDim = 0 // clamp; 0 → falls back to the platform default dim
	}
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
	// A changed embedding model/dim rebuilds the collection at the new size:
	// drop + re-enqueue in the background so the owner doesn't have to remember
	// to click Reindex (forgetting it silently stalls the embed worker).
	newRAG := community.ResolveRAG(s, h.Cfg)
	if h.RAG != nil && newRAG.Enabled && (oldRAG.EmbedModel != newRAG.EmbedModel || oldRAG.EmbedDim != newRAG.EmbedDim) {
		cid := c.ID
		go func() {
			if _, err := h.RAG.ReindexCommunity(context.Background(), cid); err != nil {
				h.Log.Error("auto-reindex after embed model change", "community", cid, "err", err)
			}
		}()
	}
	sse := render.NewSSE(w, r)
	_ = sse.PatchElementTempl(webtempl.OwnerSettingsForm(h.settingsData(r, c, s, true)))
}

// storageSignals is the datastar bag for the owner Storage card.
type storageSignals struct {
	Endpoint  string `json:"set_s3_endpoint"`
	Region    string `json:"set_s3_region"`
	Bucket    string `json:"set_s3_bucket"`
	AccessKey string `json:"set_s3_access_key"`
	SecretKey string `json:"set_s3_secret_key"`
}

// PostMigrateStorage points a community at its OWN S3 bucket (privacy opt-out)
// and kicks a background copy of its existing uploads from the platform store.
func (h *Handler) PostMigrateStorage(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in storageSignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if strings.TrimSpace(in.Bucket) == "" {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-storage-error", "A bucket name is required"))
		return
	}
	if blocked, reason := netguard.BlockedURL(in.Endpoint); blocked {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-storage-error", "Rejected S3 endpoint — "+reason))
		return
	}
	if h.Uploads == nil || h.Uploads.CommunityBlob == nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-storage-error", "Per-community storage is not available on this instance"))
		return
	}
	s, err := h.Communities.Settings(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Overlay the submitted config IN MEMORY (don't save yet). Secrets are
	// write-only: a blank field keeps the stored value.
	s.StorageBackend = "s3"
	s.S3Endpoint = in.Endpoint
	s.S3Region = in.Region
	s.S3Bucket = in.Bucket
	if strings.TrimSpace(in.AccessKey) != "" {
		s.S3AccessKey = in.AccessKey
	}
	if strings.TrimSpace(in.SecretKey) != "" {
		s.S3SecretKey = in.SecretKey
	}
	// Build the destination from the merged config and VERIFY connectivity
	// before persisting — otherwise a bad bucket/creds would flip the write
	// path (writeStoreFor) to a broken store and break every future upload.
	st := community.ResolveStorage(s, h.Cfg)
	dst, err := uploads.NewS3Blobstore(uploads.S3Config{
		Endpoint: st.S3Endpoint, Region: st.S3Region, Bucket: st.S3Bucket,
		AccessKey: st.S3AccessKey, SecretKey: st.S3SecretKey, UsePathStyle: st.UsePathStyle,
	})
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-storage-error", "Invalid S3 config: "+err.Error()))
		return
	}
	if _, err := dst.Exists(r.Context(), ".forumchat-connectivity-probe"); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-storage-error", "Could not reach the bucket — check endpoint, bucket name and credentials. Nothing was changed."))
		return
	}
	if err := h.Communities.SaveSettings(r.Context(), s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Migrate in the background (bytes-heavy); the row knows its own store via
	// store_key so reads stay correct throughout. Detached context so the copy
	// survives the request.
	cid := c.ID
	go func() {
		n, err := h.Uploads.MigrateCommunity(context.Background(), cid, dst)
		if err != nil {
			h.Log.Error("storage migrate", "community", cid, "err", err, "migrated", n)
			return
		}
		h.Log.Info("storage migrate complete", "community", cid, "migrated", n)
	}()
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
	st := community.ResolveStorage(s, h.Cfg)
	d.StorageBackend = st.Backend
	d.StorageOwnBucket = st.OwnBucket
	d.StorageMigratable = h.Cfg.SAAS && h.Uploads != nil && h.Uploads.CommunityBlob != nil
	d.S3Endpoint = s.S3Endpoint
	d.S3Region = s.S3Region
	d.S3Bucket = s.S3Bucket
	d.HasS3Secret = s.S3SecretKey != ""
	return d
}
