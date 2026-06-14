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
	UploadsMaxSize int64         `env:"UPLOADS_MAX_BYTES" envDefault:"5242880"`
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
	PresenceTTL    time.Duration `env:"PRESENCE_TTL" envDefault:"30s"`
	EditGrace      time.Duration `env:"EDIT_GRACE" envDefault:"15m"`

	// WebRTC ICE config for /rooms. Without TURN, guests behind symmetric
	// NAT (mobile carriers, corporate, CGNAT) cannot establish peer
	// connections — STUN alone is insufficient. Multiple STUN URLs may be
	// comma-separated. TURN is a single entry: leave URL empty to omit.
	STUNURLs     []string `env:"ROOMS_STUN_URLS" envSeparator:"," envDefault:"stun:stun.l.google.com:19302"`
	TURNURL      string   `env:"ROOMS_TURN_URL" envDefault:""`
	TURNUsername string   `env:"ROOMS_TURN_USERNAME" envDefault:""`
	TURNPassword string   `env:"ROOMS_TURN_PASSWORD" envDefault:""`
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
