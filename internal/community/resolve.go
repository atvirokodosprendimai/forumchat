package community

import "github.com/atvirokodosprendimai/forumchat/internal/config"

// resolve.go is the ONE place the tenant-config resolution rule lives:
//
//	effective = community.override ?? platform.env.default
//	gated by  env.kill_switch (a global *_ENABLED=false disables fleet-wide)
//
// In self-hosted mode (SAAS=false) every resolver short-circuits to env and
// ignores per-community settings — that is what keeps the single-tenant path
// byte-for-byte unchanged. The neutral Effective* structs are mapped to each
// subsystem's own shape by closures in main.go, so this package never imports
// rag / uploads (no cycles).

// EffectiveRAG is the resolved RAG config for one community. Platform is true
// when the values come from the operator's hosted compute (PLATFORM_AI_*), which
// means the embedder must be wrapped in the aiusage metering decorator.
type EffectiveRAG struct {
	Enabled      bool
	EmbedBaseURL string
	EmbedModel   string
	EmbedDim     int
	QdrantURL    string
	QdrantAPIKey string
	QdrantColl   string // per-community collection name (qdrant backend)
	Platform     bool   // served on platform compute → meter it
}

// EffectiveTranslate is the resolved translation config for one community.
// Platform is true when served on the operator's hosted compute (meter it).
type EffectiveTranslate struct {
	Enabled  bool
	BaseURL  string
	Model    string
	Platform bool
}

// EffectiveAgent is the resolved agent-generation compute for one community.
// When Platform is true, an agent's BYO provider/host/model/key is overridden by
// the operator's hosted compute and metered; when false the agent runs on its
// own configured backend (the default — Platform stays the zero value).
type EffectiveAgent struct {
	Platform bool
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
}

// EffectiveStorage is the resolved blob-storage config for one community.
type EffectiveStorage struct {
	Backend      string // "disk" | "s3"
	OwnBucket    bool   // community migrated to its own S3 bucket
	S3Endpoint   string
	S3Region     string
	S3Bucket     string
	S3AccessKey  string
	S3SecretKey  string
	UsePathStyle bool
}

// EffectiveAIEnabled reports whether AI is on for a community: the global
// kill-switch AND (in SaaS) the community's master toggle (default on when
// unset). The ≥1-enabled-agent check stays where it already is.
func EffectiveAIEnabled(s Settings, cfg config.Config) bool {
	if !cfg.AIEnabled {
		return false // platform kill-switch
	}
	if !cfg.SAAS {
		return true
	}
	return boolOr(s.AIEnabled, true)
}

// PlatformAI reports whether a community runs its AI on the operator's hosted
// compute. on is the owner's master switch (use_platform_ai); authorized is
// granted-free OR an active Stripe subscription. Both must be true (and SAAS on)
// for the resolvers to return platform compute. Self-hosted always returns
// (false, false) — there is no platform tier in single-tenant mode.
func PlatformAI(s Settings, cfg config.Config) (on, authorized bool) {
	if !cfg.SAAS {
		return false, false
	}
	on = boolOr(s.UsePlatformAI, false)
	authorized = boolOr(s.PlatformAIGrantedFree, false) || SubscriptionGrantsAccess(s.StripeSubscriptionStatus)
	return on, authorized
}

// SubscriptionGrantsAccess reports whether a Stripe subscription status entitles
// a community to platform AI. Both a fully-active and an in-trial subscription
// pay (or will pay), so both grant; past_due / canceled / unpaid / incomplete /
// paused do not.
func SubscriptionGrantsAccess(status string) bool {
	return status == "active" || status == "trialing"
}

// usePlatform reports whether the platform-compute branch applies: the kill
// switch is on, the community opted in, it is authorized, and the platform has
// the relevant endpoint configured (an unset PLATFORM_AI_* means the operator
// hasn't opened that capability, so fall through to BYO).
func usePlatform(s Settings, cfg config.Config, endpoint string) bool {
	if endpoint == "" {
		return false
	}
	on, authorized := PlatformAI(s, cfg)
	return on && authorized
}

// ResolveRAG resolves a community's RAG config. Self-hosted → env (single
// embedder + single collection). SaaS → platform compute when opted-in +
// authorized + configured (metered), else per-community override, opt-in
// (default off). The per-community Qdrant collection name is preserved on the
// platform branch so tenant isolation holds even on shared platform Qdrant.
func ResolveRAG(s Settings, cfg config.Config) EffectiveRAG {
	if !cfg.SAAS {
		return EffectiveRAG{
			Enabled:      cfg.RAGEnabled,
			EmbedBaseURL: cfg.RAGEmbedBaseURL,
			EmbedModel:   cfg.RAGEmbedModel,
			EmbedDim:     cfg.RAGEmbedDim,
			QdrantURL:    cfg.QdrantURL,
		}
	}
	if usePlatform(s, cfg, cfg.PlatformAIRAGBaseURL) {
		return EffectiveRAG{
			Enabled:      cfg.RAGEnabled && boolOr(s.RAGEnabled, true),
			EmbedBaseURL: cfg.PlatformAIRAGBaseURL,
			EmbedModel:   cfg.PlatformAIRAGModel,
			EmbedDim:     cfg.PlatformAIRAGDim,
			QdrantURL:    cfg.PlatformAIQdrantURL,
			QdrantAPIKey: cfg.PlatformAIQdrantAPIKey,
			QdrantColl:   "forumchat_" + s.CommunityID,
			Platform:     true,
		}
	}
	return EffectiveRAG{
		Enabled:      cfg.RAGEnabled && boolOr(s.RAGEnabled, false),
		EmbedBaseURL: strOr(s.RAGEmbedBaseURL, cfg.RAGEmbedBaseURL),
		EmbedModel:   strOr(s.RAGEmbedModel, cfg.RAGEmbedModel),
		EmbedDim:     intOr(s.RAGEmbedDim, cfg.RAGEmbedDim),
		QdrantURL:    strOr(s.RAGQdrantURL, cfg.QdrantURL),
		QdrantAPIKey: s.RAGQdrantAPIKey,
		QdrantColl:   strOr(s.RAGQdrantColl, "forumchat_"+s.CommunityID),
	}
}

// ResolveTranslate resolves a community's translation config. Self-hosted → env;
// SaaS → platform compute when opted-in + authorized + configured (metered),
// else per-community override, opt-in (default off).
func ResolveTranslate(s Settings, cfg config.Config) EffectiveTranslate {
	if !cfg.SAAS {
		return EffectiveTranslate{
			Enabled: cfg.TranslateEnabled,
			BaseURL: cfg.TranslateBaseURL,
			Model:   cfg.TranslateModel,
		}
	}
	if usePlatform(s, cfg, cfg.PlatformAITranslateBaseURL) {
		return EffectiveTranslate{
			Enabled:  cfg.TranslateEnabled && boolOr(s.TranslateEnabled, true),
			BaseURL:  cfg.PlatformAITranslateBaseURL,
			Model:    cfg.PlatformAITranslateModel,
			Platform: true,
		}
	}
	return EffectiveTranslate{
		Enabled: cfg.TranslateEnabled && boolOr(s.TranslateEnabled, false),
		BaseURL: strOr(s.TranslateBaseURL, cfg.TranslateBaseURL),
		Model:   strOr(s.TranslateModel, cfg.TranslateModel),
	}
}

// ResolveAgent resolves the compute backend for a community's agents. On the
// platform branch (opted-in + authorized + configured) it returns the operator's
// hosted provider/host/key with Platform=true, so the caller overrides the
// agent's BYO backend and meters it. Otherwise Platform is false and the agent
// runs on its own configured backend (the unchanged default).
//
// The model is selected by the agent's vision capability: a vision agent needs a
// vision-capable model (an image sent to a text model errors), so it uses
// PLATFORM_AI_AGENT_VISION_MODEL. If a vision agent is requested but the operator
// configured no vision model, the agent stays BYO (Platform false) rather than
// silently sending images to the text model.
func ResolveAgent(s Settings, cfg config.Config, vision bool) EffectiveAgent {
	if !usePlatform(s, cfg, cfg.PlatformAIAgentBaseURL) {
		return EffectiveAgent{}
	}
	model := cfg.PlatformAIAgentModel
	if vision {
		if cfg.PlatformAIAgentVisionModel == "" {
			return EffectiveAgent{} // no platform vision model → vision agent stays BYO
		}
		model = cfg.PlatformAIAgentVisionModel
	}
	if model == "" {
		return EffectiveAgent{} // capability not configured → BYO
	}
	return EffectiveAgent{
		Platform: true,
		Provider: cfg.PlatformAIAgentProvider,
		BaseURL:  cfg.PlatformAIAgentBaseURL,
		Model:    model,
		APIKey:   cfg.PlatformAIAgentAPIKey,
	}
}

// ResolveStorage resolves a community's blob-storage backend. Self-hosted →
// the platform default (disk/s3) with global creds. SaaS → the platform store
// by default; a community that migrated to its own bucket uses its own creds.
func ResolveStorage(s Settings, cfg config.Config) EffectiveStorage {
	platform := EffectiveStorage{
		Backend:      cfg.EffectiveStorageBackend(),
		S3Endpoint:   cfg.S3Endpoint,
		S3Region:     cfg.S3Region,
		S3Bucket:     cfg.S3Bucket,
		S3AccessKey:  cfg.S3AccessKey,
		S3SecretKey:  cfg.S3SecretKey,
		UsePathStyle: cfg.S3UsePathStyle,
	}
	if !cfg.SAAS || s.StorageBackend != "s3" || s.S3Bucket == "" {
		return platform
	}
	// Community opted out to its own bucket.
	return EffectiveStorage{
		Backend:      "s3",
		OwnBucket:    true,
		S3Endpoint:   s.S3Endpoint,
		S3Region:     strOr(s.S3Region, cfg.S3Region),
		S3Bucket:     s.S3Bucket,
		S3AccessKey:  s.S3AccessKey,
		S3SecretKey:  s.S3SecretKey,
		UsePathStyle: cfg.S3UsePathStyle,
	}
}

// JoinPolicy resolves how a public-community join is admitted: "open"
// (auto-approve) or "request" (approval queue). Only meaningful in SaaS, where
// the community owner sets it; self-hosted always returns "request" so the
// existing explore behaviour (every join is pending) is unchanged. The separate
// OPEN_REGISTRATION_AUTO_APPROVE env still governs the registration path.
func JoinPolicy(s Settings, cfg config.Config) string {
	if cfg.SAAS && (s.JoinPolicy == "open" || s.JoinPolicy == "request") {
		return s.JoinPolicy
	}
	return "request"
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func strOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func intOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}
