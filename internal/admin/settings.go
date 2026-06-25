package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
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
	s, err := h.Communities.Settings(r.Context(), c.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Capture the embedding identity BEFORE overlaying — a model/dim change
	// invalidates the Qdrant collection (sized to the old dim) and must trigger
	// a reindex, or the worker stalls on a dimension mismatch.
	oldRAG := community.ResolveRAG(s, h.Cfg)
	// When a capability runs on platform AI, the form hides AND blanks its BYO
	// infra fields (so the operator's hosts never leak to the tenant). A save
	// must therefore NOT overwrite the stored BYO override from those now-empty
	// signals — that would wipe a dormant config the owner set before switching
	// to platform AI, and the SSRF guard must skip the blank platform fields.
	trPlat := community.ResolveTranslate(s, h.Cfg).Platform
	ragPlat := oldRAG.Platform
	// A URL field left at the platform default is NOT a tenant override: the form
	// pre-fills each field with the resolved effective value, so an owner who only
	// changes, say, the join policy still re-submits the platform's default Ollama/
	// Qdrant host (which legitimately may be localhost). Normalize those back to an
	// empty override so they keep inheriting — and so the SSRF guard below inspects
	// only genuine tenant-supplied overrides, never the platform's own default host.
	trURL := overrideURL(in.TranslateBaseURL, h.Cfg.TranslateBaseURL)
	ragURL := overrideURL(in.RAGEmbedBaseURL, h.Cfg.RAGEmbedBaseURL)
	qdrantURL := overrideURL(in.RAGQdrantURL, h.Cfg.QdrantURL)
	// SSRF guard: tenant-supplied outbound URLs must not target internal hosts
	// (the platform dials them). Self-host is exempt — it legitimately uses
	// localhost daemons. Platform-served capabilities are skipped (their fields
	// are operator-managed, not tenant-supplied).
	if h.Cfg.SAAS {
		var check []string
		if !trPlat {
			check = append(check, trURL)
		}
		if !ragPlat {
			check = append(check, ragURL, qdrantURL)
		}
		for _, u := range check {
			if blocked, reason := netguard.BlockedURL(u); blocked {
				sse := render.NewSSE(w, r)
				_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-settings-error", "Rejected URL — "+reason))
				return
			}
		}
	}
	ai := in.AIEnabled
	s.AIEnabled = &ai
	if in.JoinPolicy == "open" || in.JoinPolicy == "request" {
		s.JoinPolicy = in.JoinPolicy
	}
	tr := in.TranslateEnabled
	s.TranslateEnabled = &tr
	if !trPlat {
		s.TranslateBaseURL = trURL
		s.TranslateModel = in.TranslateModel
	}
	rag := in.RAGEnabled
	s.RAGEnabled = &rag
	if !ragPlat {
		s.RAGEmbedBaseURL = ragURL
		s.RAGEmbedModel = in.RAGEmbedModel
		if in.RAGEmbedDim < 0 {
			in.RAGEmbedDim = 0 // clamp; 0 → falls back to the platform default dim
		}
		s.RAGEmbedDim = in.RAGEmbedDim
		s.RAGQdrantURL = qdrantURL
		// Write-only secret: only overwrite when the owner typed a new key, so a
		// blank field on save keeps the stored key rather than wiping it.
		if strings.TrimSpace(in.RAGQdrantAPIKey) != "" {
			s.RAGQdrantAPIKey = in.RAGQdrantAPIKey
		}
	}
	if err := h.Communities.SaveSettings(r.Context(), s); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A changed embedding model/dim rebuilds the collection at the new size:
	// drop + re-enqueue in the background so the owner doesn't have to remember
	// to click Reindex (forgetting it silently stalls the embed worker).
	newRAG := community.ResolveRAG(s, h.Cfg)
	ragMoved := oldRAG.EmbedModel != newRAG.EmbedModel || oldRAG.EmbedDim != newRAG.EmbedDim ||
		oldRAG.QdrantURL != newRAG.QdrantURL || oldRAG.QdrantColl != newRAG.QdrantColl
	if h.RAG != nil && newRAG.Enabled && ragMoved {
		// Model/dim change rebuilds the collection at the new size; a Qdrant
		// URL/collection change repopulates the new target (else search is empty
		// until content is next written). Drop + re-enqueue in the background.
		cid := c.ID
		go func() {
			if _, err := h.RAG.ReindexCommunity(context.Background(), cid); err != nil {
				h.Log.Error("auto-reindex after RAG config change", "community", cid, "err", err)
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

// deleteCommunitySignals is the owner Danger-Zone confirmation.
type deleteCommunitySignals struct {
	ConfirmSlug string `json:"set_delete_slug"`
}

// PostDeleteCommunity is the owner-facing self-serve community delete (SaaS
// Danger Zone). Like the super-admin path it requires the owner to type the
// slug back — re-checked server-side, never trusting the client — then routes
// through the shared Provision.Delete seam so ALL data (blobs + cascaded rows +
// vectors) is purged. Audit-logged. The route is owner-gated in main.go.
func (h *Handler) PostDeleteCommunity(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	var in deleteCommunitySignals
	if err := datastar.ReadSignals(r, &in); err != nil {
		http.Error(w, "bad signals: "+err.Error(), http.StatusBadRequest)
		return
	}
	sse := render.NewSSE(w, r)
	if strings.TrimSpace(in.ConfirmSlug) != c.Slug {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-delete-error",
			"The typed slug did not match \""+c.Slug+"\". Nothing was deleted."))
		return
	}
	actor := "unknown"
	if id, ok := auth.FromContext(r.Context()); ok {
		actor = id.User.Email
	}
	if err := h.Provision.Delete(r.Context(), c.ID); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-delete-error", "Delete failed: "+err.Error()))
		return
	}
	h.Log.Warn("owner deleted community (cascaded)", "actor", actor, "community_id", c.ID, "slug", c.Slug, "name", c.Name)
	_ = sse.Redirect("/")
}

// overrideURL returns the tenant override for an outbound URL. A blank field or
// one left at the platform default means "inherit the platform default", so it
// is stored as empty (and skipped by the SSRF guard) rather than pinned as an
// override equal to the default. See PostSettings for why the form re-submits
// the default.
func overrideURL(submitted, def string) string {
	submitted = strings.TrimSpace(submitted)
	if submitted == "" || submitted == def {
		return ""
	}
	return submitted
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
	// Don't leak operator infrastructure to the tenant owner. When a capability
	// runs on the platform's hosted compute, Resolve* returns the operator's
	// PLATFORM_AI_* host / model / Qdrant URL — blank those out (they would
	// otherwise also sit in the page's data-signals bag) and flag the card to
	// render a "managed by the operator" note instead of the BYO inputs.
	if tr.Platform {
		d.TranslatePlatform = true
		d.TranslateBaseURL = ""
		d.TranslateModel = ""
	}
	if rag.Platform {
		d.RAGPlatform = true
		d.RAGEmbedBaseURL = ""
		d.RAGEmbedModel = ""
		d.RAGEmbedDim = 0
		d.RAGQdrantURL = ""
		d.RAGHasQdrantKey = false
	}
	st := community.ResolveStorage(s, h.Cfg)
	d.StorageBackend = st.Backend
	d.StorageOwnBucket = st.OwnBucket
	d.StorageMigratable = h.Cfg.SAAS && h.Uploads != nil && h.Uploads.CommunityBlob != nil
	d.S3Endpoint = s.S3Endpoint
	d.S3Region = s.S3Region
	d.S3Bucket = s.S3Bucket
	d.HasS3Secret = s.S3SecretKey != ""

	// Platform AI card (SaaS only): the owner's opt-in standing + their own
	// rolling 30-day usage when active.
	if h.Cfg.SAAS {
		on, authorized := community.PlatformAI(s, h.Cfg)
		d.PlatformAIAvailable = true
		d.PlatformAIOn = on
		d.PlatformAIAuthorized = authorized
		d.PlatformAIStatus = s.PlatformAIStatus
		d.PlatformAIGrantedFree = s.PlatformAIGrantedFree != nil && *s.PlatformAIGrantedFree
		d.PlatformAISubscribed = community.SubscriptionGrantsAccess(s.StripeSubscriptionStatus)
		d.BillingEnabled = h.billingEnabled()
		if h.Usage != nil && on && authorized {
			now := time.Now()
			if rows, err := h.Usage.Rollup(r.Context(), c.ID, now.Add(-30*24*time.Hour).Unix(), now.Unix()); err == nil {
				for _, ft := range rows {
					d.PlatformUsage = append(d.PlatformUsage, webtempl.OwnerUsageRow{
						Feature: ft.Feature, Requests: ft.Requests, TokensIn: ft.TokensIn, TokensOut: ft.TokensOut,
					})
				}
			}
		}
	}
	return d
}

// PostRequestPlatformAI records the owner's opt-in to platform AI (queued for
// operator grant, or active immediately if already authorized) and morphs the
// card. Owner-gated by the route.
func (h *Handler) PostRequestPlatformAI(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	sse := render.NewSSE(w, r)
	if err := h.Communities.RequestPlatformAI(r.Context(), c.ID); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-pai-error", "Could not request: "+err.Error()))
		return
	}
	h.morphPlatformAICard(w, r, sse, c)
}

// PostCancelPlatformAI withdraws the owner's opt-in (keeping any grant/sub state)
// and morphs the card.
func (h *Handler) PostCancelPlatformAI(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	sse := render.NewSSE(w, r)
	if err := h.Communities.CancelPlatformAIRequest(r.Context(), c.ID); err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-pai-error", "Could not cancel: "+err.Error()))
		return
	}
	h.morphPlatformAICard(w, r, sse, c)
}

// PostBillingCheckout creates a Stripe Checkout Session for the community's
// paid platform-AI subscription and client-navigates the owner to Stripe.
// Owner-gated by the route; 404s when billing is unconfigured.
func (h *Handler) PostBillingCheckout(w http.ResponseWriter, r *http.Request) {
	c, ok := community.FromContext(r.Context())
	if !ok || !h.billingEnabled() {
		http.NotFound(w, r)
		return
	}
	sse := render.NewSSE(w, r)
	url, err := h.Billing.Checkout(r.Context(), c.ID, c.Slug)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-pai-error", "Could not start checkout: "+err.Error()))
		return
	}
	_ = sse.Redirect(url)
}

// morphPlatformAICard reloads settings and re-renders the #owner-platform-ai
// card AND the settings form. Toggling platform AI flips whether each capability
// is served by the operator, which flips whether the form shows (and its signals
// carry) the BYO infra inputs. Re-morphing the form keeps the two cards in sync —
// otherwise the form stays stale and a later Save could wipe dormant BYO config
// from the blanked platform-mode signals.
func (h *Handler) morphPlatformAICard(w http.ResponseWriter, r *http.Request, sse *datastar.ServerSentEventGenerator, c community.Community) {
	s, err := h.Communities.Settings(r.Context(), c.ID)
	if err != nil {
		_ = sse.PatchElementTempl(webtempl.ErrorFragment("owner-pai-error", err.Error()))
		return
	}
	d := h.settingsData(r, c, s, false)
	_ = sse.PatchElementTempl(webtempl.OwnerPlatformAICard(d))
	_ = sse.PatchElementTempl(webtempl.OwnerSettingsForm(d))
}
