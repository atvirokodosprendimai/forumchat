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

	// SuperAdminEmails is the platform super-admin allowlist. Any signed-in
	// user whose email matches (case-insensitive) gets god-mode across every
	// community: enter any /c/<slug>/admin without a membership, set roles
	// anywhere, plus the global /superadmin surface (all communities + all
	// users). Set via env, immutable at runtime. Empty (default) = none.
	SuperAdminEmails []string `env:"SUPERADMIN_EMAILS" envSeparator:","`

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

	PresenceTTL time.Duration `env:"PRESENCE_TTL" envDefault:"30s"`
	EditGrace   time.Duration `env:"EDIT_GRACE" envDefault:"15m"`

	// WebRTC ICE config for /rooms. Without TURN, guests behind symmetric
	// NAT (mobile carriers, corporate, CGNAT) cannot establish peer
	// connections — STUN alone is insufficient. Multiple STUN URLs may be
	// comma-separated. TURN is a single entry: leave URL empty to omit.
	STUNURLs     []string `env:"ROOMS_STUN_URLS" envSeparator:"," envDefault:"stun:stun.l.google.com:19302"`
	TURNURL      string   `env:"ROOMS_TURN_URL" envDefault:""`
	TURNUsername string   `env:"ROOMS_TURN_USERNAME" envDefault:""`
	TURNPassword string   `env:"ROOMS_TURN_PASSWORD" envDefault:""`

	// Projects feature flag. When false the /c/{slug}/projects routes are
	// not mounted, the nav link is hidden, and SSE streams are absent.
	// The DB tables always exist so flipping the flag never needs a
	// schema migration.
	ProjectsEnabled bool `env:"PROJECTS_ENABLED" envDefault:"false"`

	// GuestAccessEnabled toggles the Lobbies feature: token-authed
	// guest URLs into persistent text conversations, without the guest
	// needing an account. Default off so a fresh deployment is closed.
	GuestAccessEnabled bool `env:"GUEST_ACCESS_ENABLED" envDefault:"false"`

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
