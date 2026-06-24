package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/joho/godotenv"
)

type Config struct {
	Env            string        `env:"ENV" envDefault:"dev"`
	HTTPAddr       string        `env:"HTTP_ADDR" envDefault:":8080"`
	BaseURL        string        `env:"BASE_URL" envDefault:"http://localhost:8080"`
	DBPath         string        `env:"DB_PATH" envDefault:"./data/forumchat.db"`
	MigrateOnBoot  bool          `env:"MIGRATE_ON_BOOT" envDefault:"true"`
	NATSURL        string        `env:"NATS_URL" envDefault:"nats://127.0.0.1:4222"`
	SessionKey     string        `env:"SESSION_KEY" envDefault:"dev-only-change-me-32-bytes-min!!"`
	SessionMaxAge  time.Duration `env:"SESSION_MAX_AGE" envDefault:"720h"`
	UploadsDir     string        `env:"UPLOADS_DIR" envDefault:"./uploads"`
	UploadsMaxSize int64         `env:"UPLOADS_MAX_BYTES" envDefault:"104857600"`
	UploadsSignKey string        `env:"UPLOADS_SIGN_KEY" envDefault:"dev-only-uploads-sign-key-change!!"`
	SMTPHost       string        `env:"SMTP_HOST" envDefault:"127.0.0.1"`
	SMTPPort       int           `env:"SMTP_PORT" envDefault:"1025"`
	SMTPUser       string        `env:"SMTP_USER" envDefault:""`
	SMTPPass       string        `env:"SMTP_PASS" envDefault:""`
	SMTPFrom       string        `env:"SMTP_FROM" envDefault:"forumchat@localhost"`
	SMTPTLS        string        `env:"SMTP_TLS" envDefault:"auto"` // auto|starttls|tls|none
	SMTPTLSSkip    bool          `env:"SMTP_TLS_INSECURE" envDefault:"false"`
	CommunitySlug  string        `env:"COMMUNITY_SLUG" envDefault:"main"`
	CommunityName  string        `env:"COMMUNITY_NAME" envDefault:"The Community"`

	// SupportInboxSlug designates ONE hidden community as a write-only,
	// cross-tenant issue inbox: any signed-in member can file a report from
	// the global "Report issue" button; the report lands as a project_issue
	// in this community's "Inbox" project. Reporters never become members,
	// so they can only read back their OWN reports (+ replies) via
	// /report-issue — they cannot browse the inbox. Only platform
	// super-admins read the full inbox (existing god-mode at
	// /c/<slug>/projects/<inbox>/issues). Empty (default) = feature OFF: no
	// button, no routes, nothing seeded. Boot-seeds the community + Inbox
	// project when set (see cmd/app/main.go). SupportInboxName is the
	// display name used when seeding.
	SupportInboxSlug string `env:"SUPPORT_INBOX_SLUG" envDefault:""`
	SupportInboxName string `env:"SUPPORT_INBOX_NAME" envDefault:"Support"`

	// SuperAdminEmails is the platform super-admin allowlist. Any signed-in
	// user whose email matches (case-insensitive) gets god-mode across every
	// community: enter any /c/<slug>/admin without a membership, set roles
	// anywhere, plus the global /superadmin surface (all communities + all
	// users). Set via env, immutable at runtime. Empty (default) = none.
	SuperAdminEmails []string `env:"SUPERADMIN_EMAILS" envSeparator:","`

	// SAAS turns the public, unauthenticated "/" into the marketing landing
	// page. When false (default) anonymous visitors are sent straight to
	// /login instead — the app is a plain private community with no marketing
	// front door. SAAS=true also makes communities self-serve tenants:
	// owners configure their own AI/RAG/translate/storage/join-policy
	// (internal/community/resolve.go), registration is forced open, and the
	// single-tenant IMAP mailbox is disabled. See
	// spec - saas-tenant-config.
	SAAS bool `env:"SAAS" envDefault:"false"`

	// SecretsKey (32 bytes) encrypts per-community secrets at rest (Qdrant API
	// keys, S3 credentials) via internal/secretbox. Empty (dev) = passthrough,
	// values stored tagged-plaintext. Prod + SAAS without it is rejected at boot.
	SecretsKey string `env:"SECRETS_KEY" envDefault:""`

	// Storage backend for upload bytes. "" resolves to "s3" in SaaS and "disk"
	// otherwise (see EffectiveStorageBackend). The DB metadata, signing and MIME
	// logic in internal/uploads are backend-agnostic; only the blob backend
	// swaps. S3_* configure the shared platform bucket (S3-compatible:
	// AWS/MinIO/R2). Per-community own-bucket overrides live in community_settings.
	StorageBackend string `env:"STORAGE_BACKEND" envDefault:""` // "" | disk | s3
	S3Endpoint     string `env:"S3_ENDPOINT" envDefault:""`     // empty = AWS default
	S3Region       string `env:"S3_REGION" envDefault:"us-east-1"`
	S3Bucket       string `env:"S3_BUCKET" envDefault:""`
	S3AccessKey    string `env:"S3_ACCESS_KEY" envDefault:""`
	S3SecretKey    string `env:"S3_SECRET_KEY" envDefault:""`
	S3UsePathStyle bool   `env:"S3_USE_PATH_STYLE" envDefault:"true"` // true for MinIO/R2

	// SAASBrand is the product/brand name shown across the landing page (nav,
	// hero, footer, <title>, OG tags). Empty falls back to a placeholder.
	// Only meaningful when SAAS is true.
	SAASBrand string `env:"SAAS_BRAND" envDefault:""`

	// OpenRegistration lets strangers register without an invite code. When
	// false (default) /register requires a valid invite. When true the invite
	// field is optional and an empty code joins the bootstrap community.
	OpenRegistration bool `env:"OPEN_REGISTRATION" envDefault:"false"`

	// OpenRegistrationAutoApprove, when true, auto-approves every new member at
	// email-verification time (open OR invite-based signups), so nobody lands
	// in the pending queue. false (default) keeps the queue (approved_at = NULL
	// → /pending → admin approves). Independent of OpenRegistration.
	OpenRegistrationAutoApprove bool `env:"OPEN_REGISTRATION_AUTO_APPROVE" envDefault:"false"`

	// AutoVerifyEmail skips email verification entirely: registrants are
	// activated and signed in immediately, no verify link needed. Intended for
	// short demo windows (turn on, record/invite, turn off). Independent of the
	// other flags. Leave off in normal operation — it lets anyone register with
	// an unverifiable email.
	AutoVerifyEmail bool `env:"AUTO_VERIFY_EMAIL" envDefault:"false"`

	// OAuth (markbates/goth). Set a provider's client id + secret to light up
	// its "Continue with …" button on the login/register pages; leave either
	// empty (default) and that provider stays off. The redirect URI registered
	// with the provider must be BASE_URL + /auth/<provider>/callback — e.g.
	// http://localhost:8080/auth/google/callback. OAuth is a *login* method:
	// existing accounts are matched by verified email and linked automatically;
	// a brand-new email only creates an account when OPEN_REGISTRATION is on
	// (otherwise the sign-in is refused), mirroring the email/password path.
	GoogleClientID       string `env:"GOOGLE_CLIENT_ID" envDefault:""`
	GoogleClientSecret   string `env:"GOOGLE_CLIENT_SECRET" envDefault:""`
	FacebookClientID     string `env:"FACEBOOK_CLIENT_ID" envDefault:""`
	FacebookClientSecret string `env:"FACEBOOK_CLIENT_SECRET" envDefault:""`
	GitHubClientID       string `env:"GITHUB_CLIENT_ID" envDefault:""`
	GitHubClientSecret   string `env:"GITHUB_CLIENT_SECRET" envDefault:""`

	PresenceTTL time.Duration `env:"PRESENCE_TTL" envDefault:"30s"`
	EditGrace   time.Duration `env:"EDIT_GRACE" envDefault:"15m"`

	// WebRTC ICE config for /rooms. Without TURN, guests behind symmetric
	// NAT (mobile carriers, corporate, CGNAT) cannot establish peer
	// connections — STUN alone is insufficient. STUN and TURN URLs may both
	// be comma-separated so ONE credentialed TURN server can advertise
	// several transports (udp 3478, tcp 3478, turns/TLS 5349 for firewalls
	// that only allow 443). A TURN server WITHOUT username+password rejects
	// relay allocations (401) and silently falls back to STUN — set both.
	// ROOMS_TURN_URL (singular) is the deprecated alias merged into URLs.
	STUNURLs     []string `env:"ROOMS_STUN_URLS" envSeparator:"," envDefault:"stun:stun.l.google.com:19302"`
	TURNURLs     []string `env:"ROOMS_TURN_URLS" envSeparator:","`
	TURNURL      string   `env:"ROOMS_TURN_URL" envDefault:""` // deprecated: single-URL alias, merged into TURNURLs
	TURNUsername string   `env:"ROOMS_TURN_USERNAME" envDefault:""`
	TURNPassword string   `env:"ROOMS_TURN_PASSWORD" envDefault:""`
	// ForceRelay sets RTCPeerConnection iceTransportPolicy='relay' so peers
	// only ever use the TURN relay (never host/srflx). Last-resort switch for
	// networks where direct/STUN paths are unreliable; requires a working
	// TURN server or media won't flow at all.
	ForceRelay bool `env:"ROOMS_FORCE_RELAY" envDefault:"false"`

	// Projects feature flag. When false the /c/{slug}/projects routes are
	// not mounted, the nav link is hidden, and SSE streams are absent.
	// The DB tables always exist so flipping the flag never needs a
	// schema migration.
	ProjectsEnabled bool `env:"PROJECTS_ENABLED" envDefault:"false"`

	// TimeEnabled gates the time-accounting feature: the per-community
	// "Budget" page (/c/{slug}/budget, members-only) and the global personal
	// "Journal" timer (/journal). When false the routes are not mounted and
	// the nav links are hidden; the tables always exist so toggling the flag
	// never needs a schema migration.
	TimeEnabled bool `env:"TIME_ENABLED" envDefault:"false"`

	// GuestAccessEnabled toggles the Lobbies feature: token-authed
	// guest URLs into persistent text conversations, without the guest
	// needing an account. Default off so a fresh deployment is closed.
	GuestAccessEnabled bool `env:"GUEST_ACCESS_ENABLED" envDefault:"false"`

	// AIEnabled gates the Agent feature: per-community AI chat with
	// threads + history (/c/{slug}/agent) and the admin AI-config page.
	// When false the routes are not mounted and the nav links are hidden;
	// the tables always exist so toggling never needs a migration. Each
	// community still has its own ai_configs.enabled switch on top of this
	// instance flag. Default off.
	AIEnabled bool `env:"AI_ENABLED" envDefault:"false"`

	// AgentMCPAllowStdio gates external stdio MCP servers, which run arbitrary
	// host commands on this server. HTTP MCP servers are always allowed. Default
	// off — only enable on instances where community admins are trusted to run
	// local processes. The built-in internal search server is unaffected.
	AgentMCPAllowStdio bool `env:"AGENT_MCP_ALLOW_STDIO" envDefault:"false"`

	// Project-change → chat digest cadence (minutes). 0 disables the
	// worker. When > 0, every N minutes the worker checks each community
	// for projects with new attachments/issues/discussions/comments/replies
	// since the last digest and posts ONE system message in the
	// community chat with links to the changed projects. Quiet if nothing
	// changed.
	ProjectChatDigestMinutes int `env:"PROJECT_CHAT_DIGEST_MINUTES" envDefault:"5"`

	// Mailbox / IMAP ingest. When enabled, a poll worker dials a single
	// shared IMAP account (READ-ONLY: only EXAMINE + BODY.PEEK[], never
	// \Seen mutation or MOVE) every MailboxPollInterval. Matched messages
	// from per-community filters land in /inbox. Attachments are indexed
	// metadata-only; bytes fetched on demand at "Move to project" click.
	// MailboxSystemUserID is the synthetic users row credited as the
	// author of auto-created project_issues from to_issue=true filters.
	// Webhooks — per-community inbound (/hooks/<token>) and outbound (chat
	// relay) integrations. Off by default: the public /hooks route isn't
	// mounted, the admin page is hidden, and the outbound relay hook is nil.
	// WebhooksMaxBytes caps inbound payload size.
	WebhooksEnabled  bool  `env:"WEBHOOKS_ENABLED" envDefault:"false"`
	WebhooksMaxBytes int64 `env:"WEBHOOKS_MAX_BYTES" envDefault:"1048576"` // 1 MiB

	MailboxEnabled       bool          `env:"MAILBOX_ENABLED" envDefault:"false"`
	MailboxHost          string        `env:"MAILBOX_HOST" envDefault:""`
	MailboxPort          int           `env:"MAILBOX_PORT" envDefault:"993"`
	MailboxUser          string        `env:"MAILBOX_USER" envDefault:""`
	MailboxPass          string        `env:"MAILBOX_PASS" envDefault:""`
	MailboxTLS           string        `env:"MAILBOX_TLS" envDefault:"tls"` // tls | starttls | none
	MailboxPollInterval  time.Duration `env:"MAILBOX_POLL_INTERVAL" envDefault:"2m"`
	MailboxAttachmentMax int64         `env:"MAILBOX_ATTACHMENT_MAX" envDefault:"26214400"` // 25 MiB
	MailboxSystemUserID  string        `env:"MAILBOX_SYSTEM_USER_ID" envDefault:""`
	// MAILBOX_RESCAN_ON_BOOT resets every folder's last_uid to 0 at
	// startup so the next poll cycle re-scans everything from UID 1.
	// Useful after enabling a new filter that should have matched
	// historical mail. Set true once, restart, then set false.
	MailboxRescanOnBoot bool `env:"MAILBOX_RESCAN_ON_BOOT" envDefault:"false"`

	// RAG — semantic (vector) search over community-public content, the async
	// sibling of the FTS5 search_fts index. When enabled, a background worker
	// drains embed_outbox (written by SQL triggers), embeds each changed row via
	// Ollama, and upserts into a pluggable vector store (chromem-go now, qdrant
	// later). The agent's internal MCP gains a `rag_search` tool. Default off:
	// without a reachable embedder the worker would just log errors. The
	// embedder is independent of the per-agent Ollama config — RAG has its own
	// endpoint/model so embedding and chat can point at different daemons.
	RAGEnabled       bool   `env:"RAG_ENABLED" envDefault:"false"`
	RAGBackend       string `env:"RAG_BACKEND" envDefault:""` // "" | chromem | qdrant
	RAGStorePath     string `env:"RAG_DB_PATH" envDefault:"./data/rag"`
	RAGEmbedBaseURL  string `env:"RAG_EMBED_BASEURL" envDefault:"http://localhost:11434"`
	RAGEmbedModel    string `env:"RAG_EMBED_MODEL" envDefault:"bge-m3"`
	RAGEmbedDim      int    `env:"RAG_EMBED_DIM" envDefault:"1024"`
	RAGChunkTokens   int    `env:"RAG_CHUNK_TOKENS" envDefault:"2800"`  // primary body window
	RAGChunkOverlap  int    `env:"RAG_CHUNK_OVERLAP" envDefault:"400"`  // context bled in on each side
	RAGWorkerSeconds int    `env:"RAG_WORKER_INTERVAL" envDefault:"10"` // drain cadence, seconds
	RAGWorkerBatch   int    `env:"RAG_WORKER_BATCH" envDefault:"64"`
	RAGSearchDefault int    `env:"RAG_SEARCH_DEFAULT_LIMIT" envDefault:"8"`
	// QdrantURL is read only when RAG_BACKEND=qdrant (reserved; the chromem
	// backend ignores it).
	QdrantURL string `env:"QDRANT_URL" envDefault:""`

	// Translate powers the interactive /translate composer typeahead: type
	// "/translate <text>" in chat and a popup offers up to 3 English
	// translations (auto-detected source language) to send as yourself. Like
	// RAG's embedder it talks to Ollama directly and has its OWN endpoint +
	// model — independent of the per-community AI agents (ai_agents) — so
	// translation and chat can point at different daemons. Disabled by default;
	// with TRANSLATE_MODEL empty the command is silently inert.
	TranslateEnabled bool   `env:"TRANSLATE_ENABLED" envDefault:"false"`
	TranslateBaseURL string `env:"TRANSLATE_BASEURL" envDefault:"http://localhost:11434"`
	TranslateModel   string `env:"TRANSLATE_MODEL" envDefault:""`

	// Platform AI (SaaS only) — the operator's OWN hosted AI compute, offered to
	// communities that opt into "use system-wide settings" and are authorized
	// (super-admin grant OR active Stripe subscription). It is a DISTINCT
	// namespace from the BYO RAG_*/TRANSLATE_* env above (which remain
	// per-community inheritance defaults): leaving these unset keeps the
	// 2026-06-23 invariant intact — no community can use platform compute, so the
	// operator pays zero. Every request served on this compute is metered into
	// ai_usage_events (internal/aiusage). See
	// eidos/spec - saas-platform-ai …
	PlatformAIRAGBaseURL       string `env:"PLATFORM_AI_RAG_BASEURL" envDefault:""`
	PlatformAIRAGModel         string `env:"PLATFORM_AI_RAG_MODEL" envDefault:""`
	PlatformAIRAGDim           int    `env:"PLATFORM_AI_RAG_DIM" envDefault:"0"`
	PlatformAIQdrantURL        string `env:"PLATFORM_AI_QDRANT_URL" envDefault:""`
	PlatformAIQdrantAPIKey     string `env:"PLATFORM_AI_QDRANT_API_KEY" envDefault:""`
	PlatformAITranslateBaseURL string `env:"PLATFORM_AI_TRANSLATE_BASEURL" envDefault:""`
	PlatformAITranslateModel   string `env:"PLATFORM_AI_TRANSLATE_MODEL" envDefault:""`
	PlatformAIAgentProvider    string `env:"PLATFORM_AI_AGENT_PROVIDER" envDefault:""`
	PlatformAIAgentBaseURL     string `env:"PLATFORM_AI_AGENT_BASEURL" envDefault:""`
	PlatformAIAgentModel       string `env:"PLATFORM_AI_AGENT_MODEL" envDefault:""`        // text model (e.g. glm-5.2)
	PlatformAIAgentVisionModel string `env:"PLATFORM_AI_AGENT_VISION_MODEL" envDefault:""` // vision model (e.g. gemma4); used for agents with vision on
	PlatformAIAgentAPIKey      string `env:"PLATFORM_AI_AGENT_API_KEY" envDefault:""`

	// Stripe billing for PAID platform-AI access (SaaS). All three required to
	// enable: the secret key (create checkout), the price id (the subscription
	// product to sell), and the webhook secret (verify Stripe's callbacks — the
	// sole authority on subscription state). Any unset → billing off, no
	// Subscribe button, no webhook route; communities reach platform AI only via
	// a super-admin free grant.
	StripeSecretKey         string `env:"STRIPE_SECRET_KEY" envDefault:""`
	StripeWebhookSecret     string `env:"STRIPE_WEBHOOK_SECRET" envDefault:""`
	StripePlatformAIPriceID string `env:"STRIPE_PLATFORM_AI_PRICE_ID" envDefault:""`

	// Web Push (VAPID) — leave VAPID_PRIVATE/PUBLIC empty to auto-generate
	// on first boot and persist to VAPID_KEYS_FILE so subsequent boots
	// keep the same key pair (otherwise every browser subscription would
	// stop working after a restart). VAPID_SUBJECT is the mailto: or URL
	// the push service shows in dispatch logs.
	VAPIDPublic   string `env:"VAPID_PUBLIC" envDefault:""`
	VAPIDPrivate  string `env:"VAPID_PRIVATE" envDefault:""`
	VAPIDSubject  string `env:"VAPID_SUBJECT" envDefault:"mailto:admin@example.com"`
	VAPIDKeysFile string `env:"VAPID_KEYS_FILE" envDefault:"./data/vapid.json"`
}

func (c Config) IsProd() bool { return strings.EqualFold(c.Env, "prod") }

// EffectiveStorageBackend resolves the platform default blob backend: an explicit
// STORAGE_BACKEND wins, otherwise SaaS defaults to s3 (Qdrant+S3 are the SaaS
// path) and self-hosted defaults to disk (local ./uploads).
func (c Config) EffectiveStorageBackend() string {
	switch c.StorageBackend {
	case "disk", "s3":
		return c.StorageBackend
	}
	if c.SAAS {
		return "s3"
	}
	return "disk"
}

// EffectiveRAGBackend resolves the vector store: explicit RAG_BACKEND wins,
// otherwise SaaS defaults to qdrant (per-community collections) and self-hosted
// defaults to chromem (embedded, single collection).
func (c Config) EffectiveRAGBackend() string {
	switch c.RAGBackend {
	case "chromem", "qdrant":
		return c.RAGBackend
	}
	if c.SAAS {
		return "qdrant"
	}
	return "chromem"
}

func Load() (Config, error) {
	_ = godotenv.Load()
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	if cfg.IsProd() {
		if cfg.SessionKey == "" || strings.Contains(cfg.SessionKey, "dev-only") {
			return Config{}, fmt.Errorf("SESSION_KEY must be set in production")
		}
		if strings.Contains(cfg.UploadsSignKey, "dev-only") {
			return Config{}, fmt.Errorf("UPLOADS_SIGN_KEY must be set in production")
		}
	}
	// SaaS mode reshapes a few globals so the single-tenant path stays the
	// default and SaaS is opt-in via one flag.
	if cfg.SAAS {
		// Registration is open in SaaS — strangers sign up and join/create
		// communities; the per-community join_policy decides approval.
		cfg.OpenRegistration = true
		// Single-tenant inbound-mail ingest is not a SaaS feature.
		cfg.MailboxEnabled = false
		// Per-community secrets are stored at rest; require a real key in prod.
		if cfg.IsProd() && cfg.SecretsKey == "" {
			return Config{}, fmt.Errorf("SECRETS_KEY (32 bytes) must be set when SAAS=true in production")
		}
	}
	return cfg, nil
}

func NewLogger(cfg Config) *slog.Logger {
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	if cfg.IsProd() {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		opts.Level = slog.LevelDebug
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}
