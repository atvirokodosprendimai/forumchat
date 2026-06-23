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

// EffectiveRAG is the resolved RAG config for one community.
type EffectiveRAG struct {
	Enabled      bool
	EmbedBaseURL string
	EmbedModel   string
	EmbedDim     int
	QdrantURL    string
	QdrantAPIKey string
	QdrantColl   string // per-community collection name (qdrant backend)
}

// EffectiveTranslate is the resolved translation config for one community.
type EffectiveTranslate struct {
	Enabled bool
	BaseURL string
	Model   string
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

// ResolveRAG resolves a community's RAG config. Self-hosted → env (single
// embedder + single collection). SaaS → per-community override, opt-in
// (default off), with a dedicated collection name.
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

// ResolveTranslate resolves a community's translation config. Self-hosted →
// env; SaaS → per-community override, opt-in (default off).
func ResolveTranslate(s Settings, cfg config.Config) EffectiveTranslate {
	if !cfg.SAAS {
		return EffectiveTranslate{
			Enabled: cfg.TranslateEnabled,
			BaseURL: cfg.TranslateBaseURL,
			Model:   cfg.TranslateModel,
		}
	}
	return EffectiveTranslate{
		Enabled: cfg.TranslateEnabled && boolOr(s.TranslateEnabled, false),
		BaseURL: strOr(s.TranslateBaseURL, cfg.TranslateBaseURL),
		Model:   strOr(s.TranslateModel, cfg.TranslateModel),
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
