package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"io"

	"github.com/andybalholm/brotli"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
	"github.com/klauspost/compress/zstd"
	"github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/admin"
	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/atvirokodosprendimai/forumchat/internal/agent/mcpx"
	"github.com/atvirokodosprendimai/forumchat/internal/agentlimit"
	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/billing"
	"github.com/atvirokodosprendimai/forumchat/internal/bookmarks"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/chatagents"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/dashboard"
	"github.com/atvirokodosprendimai/forumchat/internal/dataexport"
	"github.com/atvirokodosprendimai/forumchat/internal/debuglog"
	"github.com/atvirokodosprendimai/forumchat/internal/explore"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
	"github.com/atvirokodosprendimai/forumchat/internal/history"
	"github.com/atvirokodosprendimai/forumchat/internal/httpx"
	"github.com/atvirokodosprendimai/forumchat/internal/invites"
	"github.com/atvirokodosprendimai/forumchat/internal/lobbies"
	"github.com/atvirokodosprendimai/forumchat/internal/mailbox"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/netguard"
	"github.com/atvirokodosprendimai/forumchat/internal/notes"
	"github.com/atvirokodosprendimai/forumchat/internal/pastes"
	"github.com/atvirokodosprendimai/forumchat/internal/presence"
	"github.com/atvirokodosprendimai/forumchat/internal/privatemsg"
	"github.com/atvirokodosprendimai/forumchat/internal/projects"
	"github.com/atvirokodosprendimai/forumchat/internal/provision"
	"github.com/atvirokodosprendimai/forumchat/internal/push"
	"github.com/atvirokodosprendimai/forumchat/internal/rag"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/rooms"
	"github.com/atvirokodosprendimai/forumchat/internal/search"
	"github.com/atvirokodosprendimai/forumchat/internal/secretbox"
	"github.com/atvirokodosprendimai/forumchat/internal/sendtoken"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/superadmin"
	"github.com/atvirokodosprendimai/forumchat/internal/support"
	"github.com/atvirokodosprendimai/forumchat/internal/timebudget"
	"github.com/atvirokodosprendimai/forumchat/internal/todos"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	"github.com/atvirokodosprendimai/forumchat/internal/webhooks"
	"github.com/atvirokodosprendimai/forumchat/internal/worklog"
	"github.com/atvirokodosprendimai/forumchat/web"
	webtempl "github.com/atvirokodosprendimai/forumchat/web/templ"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := config.NewLogger(cfg)
	log.Info("forumchat booting", "env", cfg.Env, "addr", cfg.HTTPAddr)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sqlite.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if cfg.MigrateOnBoot {
		log.Info("running migrations", "path", cfg.DBPath)
		if err := sqlite.Migrate(ctx, db); err != nil {
			return err
		}
	}

	// Platform debug recorder — in-memory on/off switch (off at boot), gates
	// payload capture into debug_logs. Shared by webhooks (capture) and the
	// super-admin surface (toggle + view + clear).
	debugRec := debuglog.New(db, log)

	// usageRec meters PLATFORM AI compute: every RAG-embed / translate / agent
	// request a community runs on the operator's hosted AI (the "use system-wide
	// settings" opt-in) appends one ai_usage_events row. BYO requests stay bare
	// and record nothing. See eidos/spec - saas-platform-ai …
	usageRec := aiusage.New(db, log)

	// Secrets box seals per-community tenant secrets (Qdrant/S3 keys) at rest.
	// Empty SECRETS_KEY (dev) yields a passthrough; prod+SaaS requires a real
	// key (enforced in config.Load).
	secrets, err := secretbox.New(cfg.SecretsKey)
	if err != nil {
		return fmt.Errorf("secretbox: %w", err)
	}
	// config.Load only rejects an empty key for prod+SaaS. A prod self-host with
	// no key gets passthrough — any secret stored at rest would be PLAINTEXT.
	// Nothing seals secrets in self-host today, but warn loudly so a future
	// secret-writing feature can't ship plaintext-at-rest unnoticed.
	if cfg.IsProd() && cfg.SecretsKey == "" {
		log.Warn("SECRETS_KEY is not set in production — secret-at-rest encryption is DISABLED (passthrough); set SECRETS_KEY (32 bytes) before storing any tenant secrets")
	}

	// sendSigner mints the per-user, SSE-delivered liveness token required on
	// member-write content sends (chat/forum/PM/discussion). Keyed off the
	// session secret so it's stateless + restart-proof. See internal/sendtoken.
	sendSigner := sendtoken.New(cfg.SessionKey)

	cRepo := community.NewRepo(db)
	cRepo.Secrets = secrets
	bootCommunity, err := cRepo.BootstrapOrFetch(ctx, cfg.CommunitySlug, cfg.CommunityName)
	if err != nil {
		return fmt.Errorf("bootstrap community: %w", err)
	}
	log.Info("community ready", "slug", bootCommunity.Slug, "id", bootCommunity.ID)

	// Stripe billing for paid platform AI (SaaS). Inert unless all three Stripe
	// secrets are set — then the owner Subscribe button + /billing/webhook mount.
	// cRepo is the subscription Store (the resolver reads the status it writes).
	billingSvc := billing.New(cfg.StripeSecretKey, cfg.StripeWebhookSecret, cfg.StripePlatformAIPriceID, cfg.BaseURL, cRepo, log)

	nc, err := natsx.Connect(cfg.NATSURL, log)
	if err != nil {
		log.Warn("nats connect failed, continuing without nats", "err", err)
	}
	if nc != nil {
		defer nc.Drain()
	}

	// Auth wiring.
	aRepo := auth.NewRepo(db)
	var mailer auth.Mailer
	if cfg.SMTPHost != "" && cfg.SMTPPort > 0 {
		mailer = &auth.SMTPMailer{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			User: cfg.SMTPUser, Pass: cfg.SMTPPass,
			From:    cfg.SMTPFrom,
			TLSMode: cfg.SMTPTLS, TLSSkip: cfg.SMTPTLSSkip,
			Log: log,
		}
	} else {
		mailer = &auth.LogMailer{Log: log}
	}
	svc := &auth.Service{
		Repo:                        aRepo,
		Mailer:                      mailer,
		BaseURL:                     cfg.BaseURL,
		VerifyTTL:                   48 * time.Hour,
		InviteTTL:                   30 * 24 * time.Hour,
		Log:                         log,
		CommunityID:                 bootCommunity.ID,
		OpenRegistration:            cfg.OpenRegistration,
		OpenRegistrationAutoApprove: cfg.OpenRegistrationAutoApprove,
		AutoVerifyEmail:             cfg.AutoVerifyEmail,
	}
	sessions := auth.NewSessionManager(cfg.SessionMaxAge, cfg.IsProd())
	superAdmins := auth.NewSuperAdminSet(cfg.SuperAdminEmails)
	if len(superAdmins) > 0 {
		log.Info("platform super-admins configured", "count", len(superAdmins))
	}
	auth.LoaderLog = log
	// Persistent sessions in SQLite so users stay signed in across restarts.
	sessions.Store = auth.NewSQLStore(ctx, db)
	// Social login (goth). Returns the configured providers; empty = OAuth off.
	oauthProviders := auth.SetupOAuth(auth.OAuthConfig{
		BaseURL:              cfg.BaseURL,
		Secure:               cfg.IsProd(),
		SessionKey:           cfg.SessionKey,
		GoogleClientID:       cfg.GoogleClientID,
		GoogleClientSecret:   cfg.GoogleClientSecret,
		FacebookClientID:     cfg.FacebookClientID,
		FacebookClientSecret: cfg.FacebookClientSecret,
		GitHubClientID:       cfg.GitHubClientID,
		GitHubClientSecret:   cfg.GitHubClientSecret,
	})
	if len(oauthProviders) > 0 {
		log.Info("oauth login enabled", "providers", len(oauthProviders))
	}
	authHandler := &auth.Handler{
		Svc:            svc,
		Repo:           aRepo,
		Sessions:       sessions,
		CommunityID:    bootCommunity.ID,
		CommunityName:  bootCommunity.Name,
		Log:            log,
		OAuthProviders: oauthProviders,
	}

	r := chi.NewRouter()
	r.Use(httpx.Recover(log))
	r.Use(httpx.RequestLogger(log))
	r.Use(newCompressor().Handler)
	r.Use(htmlContentType)
	r.Use(sessions.LoadAndSave)
	r.Use(auth.Loader(sessions, aRepo, superAdmins))
	// Stash the request path so the sidebar can mark the active link
	// server-side (replaces the client DOM-walk that used to live in nav.js).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(webtempl.WithCurrentPath(req.Context(), req.URL.Path)))
		})
	})
	// Stash the viewer's switchable communities for the top-bar switcher
	// dropdown (one-click swap). Only approved, non-banned memberships are
	// listed — those are the ones a click can actually enter. Leaf-package
	// rule (§4.13): web/templ reads the list off ctx, the mapping lives here.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id, ok := auth.FromContext(req.Context())
			if !ok {
				next.ServeHTTP(w, req)
				return
			}
			rows, err := cRepo.ListForUser(req.Context(), id.User.ID)
			if err != nil {
				next.ServeHTTP(w, req)
				return
			}
			comms := make([]webtempl.TopNavCommunity, 0, len(rows))
			for _, row := range rows {
				if !row.IsApproved || row.IsBanned {
					continue
				}
				comms = append(comms, webtempl.TopNavCommunity{Slug: row.Community.Slug, Name: row.Community.Name})
			}
			ctx := context.WithValue(req.Context(), webtempl.TopNavCommunitiesCtxKey(), comms)
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})

	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		v := webtempl.Viewer{CommunityName: bootCommunity.Name}
		if id, ok := auth.FromContext(req.Context()); ok {
			v.IsAuthed = true
			v.DisplayName = id.Membership.DisplayName
			v.Role = string(id.Membership.Role)
		}
		_ = webtempl.NotFoundPage(v).Render(req.Context(), w)
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed"))
	})

	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	staticFS, err := fs.Sub(web.Static, "static")
	if err != nil {
		return fmt.Errorf("static assets: %w", err)
	}
	// Every URL is content-hashed (?v=<sha>), so the cached copy can never go
	// stale for a given URL — mark it immutable so the browser reuses it with
	// zero revalidation. That kills the per-navigation re-fetch of app.css.
	r.Handle("/static/*", http.StripPrefix("/static/", immutableStatic(http.FileServerFS(staticFS))))

	// Serve the push service worker from the site root so it can claim
	// the whole '/' scope. Without this, registering /static/sw.js
	// confines its scope to /static/* and the push events never fire on
	// app routes. Also set Service-Worker-Allowed for belt-and-braces.
	// NOT immutable — a service worker must revalidate so updates land.
	r.Get("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, staticFS, "sw.js")
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Rate-limit auth endpoints (10 req/min/IP) and chat send (30 req/min/user).
	r.Group(func(r chi.Router) {
		r.Use(httprate.LimitByIP(10, time.Minute))
		r.Post("/login", authHandler.PostLogin)
		r.Post("/login/check", authHandler.PostLoginCheck)
		r.Post("/login/back", authHandler.PostLoginBack)
		r.Post("/login/magic", authHandler.PostLoginMagic)
		r.Post("/register", authHandler.PostRegister)
		r.Post("/register-as-admin", authHandler.PostRegisterAsAdmin)
	})
	r.Get("/register", authHandler.GetRegister)
	r.Get("/register-as-admin", authHandler.GetRegisterAsAdmin)
	r.Get("/login", authHandler.GetLogin)
	r.Get("/login/magic", authHandler.GetLoginMagic)
	r.Get("/verify", authHandler.GetVerify)
	r.Post("/logout", authHandler.PostLogout)
	// Account-erasure confirmation is token-gated, not session-gated (the link is
	// emailed behind a password step), so these stay public like the magic link.
	r.Get("/profile/delete/confirm", authHandler.GetDeleteConfirm)
	r.Post("/profile/delete/confirm", authHandler.PostDeleteConfirm)
	r.Get("/goodbye", authHandler.GetGoodbye)
	// OAuth (goth) — begin + provider callback. Mounted only when at least one
	// provider has credentials, so unconfigured installs expose no dead routes.
	if len(oauthProviders) > 0 {
		r.Get("/auth/{provider}", authHandler.GetOAuthBegin)
		r.Get("/auth/{provider}/callback", authHandler.GetOAuthCallback)
	}

	uploadStore := uploads.NewStore(db, cfg.UploadsDir, cfg.UploadsMaxSize, cfg.UploadsSignKey)
	// Platform blob backend: disk by default, S3 when STORAGE_BACKEND=s3 (the
	// SaaS default). The disk Dir stays the local scratch path for upload
	// streaming even when bytes land in S3.
	if cfg.EffectiveStorageBackend() == "s3" {
		if cfg.S3Bucket == "" {
			log.Warn("uploads: STORAGE_BACKEND=s3 but S3_BUCKET is unset — falling back to local disk")
		} else {
			s3, err := uploads.NewS3Blobstore(uploads.S3Config{
				Endpoint:     cfg.S3Endpoint,
				Region:       cfg.S3Region,
				Bucket:       cfg.S3Bucket,
				AccessKey:    cfg.S3AccessKey,
				SecretKey:    cfg.S3SecretKey,
				UsePathStyle: cfg.S3UsePathStyle,
			})
			if err != nil {
				return fmt.Errorf("s3 blobstore: %w", err)
			}
			uploadStore.Blob = s3
			log.Info("uploads: using S3 backend", "bucket", cfg.S3Bucket, "endpoint", cfg.S3Endpoint)
		}
	}
	// Per-community own-bucket resolver (SaaS privacy opt-out): a community that
	// migrated to its own S3 reads/writes there; everyone else uses the default
	// store. ResolveStorage returns OwnBucket only in SaaS, so this is inert
	// self-host. One extra settings read per resolve — acceptable; PK-indexed.
	if cfg.SAAS {
		uploadStore.CommunityBlob = func(ctx context.Context, communityID string) (uploads.Blobstore, string, error) {
			s, err := cRepo.Settings(ctx, communityID)
			if err != nil {
				return nil, "", err
			}
			st := community.ResolveStorage(s, cfg)
			if !st.OwnBucket {
				return nil, "", nil
			}
			bs, err := uploads.NewS3Blobstore(uploads.S3Config{
				Endpoint: st.S3Endpoint, Region: st.S3Region, Bucket: st.S3Bucket,
				AccessKey: st.S3AccessKey, SecretKey: st.S3SecretKey, UsePathStyle: st.UsePathStyle,
			})
			if err != nil {
				return nil, "", err
			}
			return bs, uploads.StoreKeyCommunity, nil
		}
	}
	// ----- Data export (owner-initiated "download all my data") -----------
	// Builds a ZIP of a community's data + media behind a 7-day signed-token
	// URL. Reuses uploadStore for media bytes; the zip lives under a sibling
	// exports/ dir. The Worker (started with the other background workers)
	// drains pending requests and sweeps expired artifacts.
	exportSvc := &dataexport.Service{
		Repo:  dataexport.NewRepo(db),
		DB:    db,
		Media: uploadStore,
		Dir:   filepath.Join(cfg.UploadsDir, "exports"),
		Log:   log,
	}
	exportHandler := &dataexport.Handler{Svc: exportSvc, Log: log}

	uploadHandler := &uploads.Handler{
		Store:       uploadStore,
		CommunityID: bootCommunity.ID,
		Log:         log,
		Sessions:    sessions, // lets project-share guests view images
		// Gate authed viewers to the upload's community (super-admin bypass
		// is handled in the handler). Prevents cross-tenant media reads.
		MemberOf: func(ctx context.Context, userID, communityID string) bool {
			_, err := aRepo.MembershipFor(ctx, userID, communityID)
			return err == nil
		},
	}

	chatRepo := chat.NewRepo(db)
	chatSvc := chat.NewService(chatRepo)
	chatBus := chat.NewBus()
	chatNewMsgBus := chat.NewBus()
	chatHandler := &chat.Handler{
		Svc:       chatSvc,
		Repo:      chatRepo,
		NATS:      nc,
		Bus:       chatBus,
		NewMsgBus: chatNewMsgBus,
		Uploads:   uploadStore,
		AuthRepo:  aRepo,
		Flood:     agentlimit.New(), // per-user chat flood control

		BaseURL:       cfg.BaseURL,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}

	presenceTracker := presence.New(cfg.PresenceTTL)
	go func() {
		t := time.NewTicker(cfg.PresenceTTL / 2)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				presenceTracker.Sweep()
			}
		}
	}()
	presenceHandler := &presence.Handler{
		Tracker: presenceTracker, Members: aRepo, Blocks: aRepo, CommunityID: bootCommunity.ID, Log: log,
	}
	// chatHandler is built before the tracker exists; wire the roster
	// nudge now so block/unblock re-renders the presence sidebar.
	chatHandler.Roster = presenceTracker

	forumRepo := forum.NewRepo(db)
	forumSvc := forum.NewService(forumRepo, cfg.EditGrace)
	forumBus := forum.NewBus()
	forumHandler := &forum.Handler{
		Svc:           forumSvc,
		Repo:          forumRepo,
		Chat:          chatSvc,
		ChatRepo:      chatRepo,
		ChatBus:       chatBus,
		ChatNewMsgBus: chatNewMsgBus,
		Bus:           forumBus,
		NATS:          nc,
		Uploads:       uploadStore,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		BaseURL:       cfg.BaseURL,
		Log:           log,
	}

	// provSvc is the single create → seed-#general → seed-first-member sequence,
	// shared by the super-admin, per-community admin, and SaaS self-serve create
	// paths. SeedChannel is a closure so internal/provision stays free of a chat
	// import (no cycle).
	provSvc := &provision.Service{
		Communities: cRepo,
		Auth:        aRepo,
		SeedChannel: func(ctx context.Context, communityID string) error {
			_, err := chatRepo.EnsureDefaultChannel(ctx, communityID)
			return err
		},
		Blobs: uploadStore, // purge upload blobs on community delete (ALL data)
		Log:   log,
	}

	// Account erasure (self-serve delete) reuses provision to delete the user's
	// solo-owned communities and the upload store to purge their owned blobs.
	svc.Communities = provSvc
	svc.Uploads = uploadStore

	adminHandler := &admin.Handler{
		Repo:          aRepo,
		Svc:           svc,
		Chat:          chatHandler,
		Communities:   cRepo,
		Provision:     provSvc,
		Roster:        presenceTracker,
		Mail:          mailer,
		BaseURL:       cfg.BaseURL,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Cfg:           cfg,
		Uploads:       uploadStore,
		Usage:         usageRec,
		Billing:       billingSvc,
		Log:           log,
	}

	dashboardHandler := &dashboard.Handler{Communities: cRepo, Auth: aRepo, Cfg: cfg, Provision: provSvc, Log: log}
	exploreHandler := &explore.Handler{Communities: cRepo, AuthRepo: aRepo, Sessions: sessions, Cfg: cfg, Log: log}

	todosHandler := &todos.Handler{Repo: todos.NewRepo(db), ChatRepo: chatRepo, Forum: forumRepo, Log: log}

	projectsRepo := projects.NewRepo(db)
	projectsBus := projects.NewBus()
	projectsSvc := projects.NewService(projectsRepo, projectsBus, uploadStore, cfg.EditGrace)
	projectsHandler := &projects.Handler{
		Repo:     projectsRepo,
		Svc:      projectsSvc,
		Bus:      projectsBus,
		Uploads:  uploadStore,
		Sessions: sessions,
		AuthRepo: aRepo,
		ChatRepo: chatRepo,
		ChatBus:  chatBus,
		Log:      log,
	}
	projectsHandler.SetCommunityLookup(func(ctx context.Context, id string) (*projects.CommunityRef, error) {
		c, err := cRepo.ByID(ctx, id)
		if err != nil {
			return nil, err
		}
		return &projects.CommunityRef{ID: c.ID, Slug: c.Slug, Name: c.Name}, nil
	})
	webtempl.ProjectsEnabled = cfg.ProjectsEnabled
	webtempl.OpenRegistration = cfg.OpenRegistration
	webtempl.SaaSEnabled = cfg.SAAS
	webtempl.SaaSBrand = cfg.SAASBrand

	// ----- support inbox (hidden cross-tenant write-only issue inbox) ------
	// ONE designated community (SUPPORT_INBOX_SLUG) collects reports filed
	// from the global "Report issue" button. Reporters never join it, so
	// they only read back their own reports; only platform super-admins read
	// the full inbox (god-mode at /c/<slug>/projects/<inbox>/issues). Empty
	// slug = feature off (no routes, no nav link, nothing seeded). The Inbox
	// project is created lazily on the first report.
	var supportHandler *support.Handler
	if cfg.SupportInboxSlug != "" {
		supportCommunity, err := cRepo.BootstrapOrFetch(ctx, cfg.SupportInboxSlug, cfg.SupportInboxName)
		if err != nil {
			return fmt.Errorf("bootstrap support inbox community: %w", err)
		}
		supportHandler = support.New(supportCommunity.ID, cRepo, projectsSvc, projectsRepo, log)
		webtempl.SupportInboxEnabled = true
		log.Info("support inbox ready", "slug", supportCommunity.Slug, "id", supportCommunity.ID)
	}

	// ----- RAG (semantic vector search) ------------------------------------
	// Built here so the agent's internal MCP can expose it as `rag_search`. The
	// drain worker is started later with the other workers (digestCtx). When
	// disabled, ragSvc stays nil and the rag_search tool is never registered;
	// the embed_outbox triggers still fire but nothing drains them (bounded —
	// see migration 00039).
	var ragSvc *rag.Service
	if cfg.RAGEnabled {
		var ragStore rag.Store
		ragBackend := cfg.EffectiveRAGBackend()
		switch ragBackend {
		case "chromem":
			cs, err := rag.NewChromemStore(cfg.RAGStorePath)
			if err != nil {
				log.Error("rag: open chromem store", "path", cfg.RAGStorePath, "err", err)
				os.Exit(1)
			}
			ragStore = cs
		case "qdrant":
			// Per-community collections on Qdrant (the SaaS path). Resolve maps a
			// community to its connection — BYO Qdrant URL/key + collection,
			// falling back to the platform QDRANT_URL.
			qs := rag.NewQdrantStore(cfg.QdrantURL, "", log)
			qs.Resolve = func(ctx context.Context, communityID string) rag.QdrantConn {
				s, err := cRepo.Settings(ctx, communityID)
				if err != nil {
					return rag.QdrantConn{}
				}
				r := community.ResolveRAG(s, cfg)
				return rag.QdrantConn{URL: r.QdrantURL, APIKey: r.QdrantAPIKey, Collection: r.QdrantColl}
			}
			ragStore = qs
		default:
			log.Error("rag: unsupported RAG_BACKEND", "backend", ragBackend)
			os.Exit(1)
		}
		ragSvc = rag.NewService(
			rag.NewRepo(db),
			rag.NewOllamaEmbedder(cfg.RAGEmbedBaseURL, cfg.RAGEmbedModel, cfg.RAGEmbedDim),
			ragStore,
			rag.ChunkConfig{BodyTokens: cfg.RAGChunkTokens, Overlap: cfg.RAGChunkOverlap},
			log,
		)
		// Per-community embedder (SaaS): each community's own model / Ollama host
		// / dim. A model with a different vector size sizes that community's
		// Qdrant collection accordingly. Self-host leaves this nil → single
		// embedder, unchanged. Cache by (host|model|dim) to reuse HTTP clients.
		if cfg.SAAS {
			var embCache sync.Map
			var warnedPlatformGap sync.Map // communityID → once, to keep the warning out of the per-embed hot path
			ragSvc.EmbedderFor = func(ctx context.Context, communityID string) (rag.Embedder, error) {
				s, err := cRepo.Settings(ctx, communityID)
				if err != nil {
					return nil, err
				}
				r := community.ResolveRAG(s, cfg)
				// Silent-fallback guard: a community that opted into platform AI and is
				// authorized but whose embedder did NOT resolve to platform compute can
				// only mean PLATFORM_AI_RAG_BASEURL is unset, so it is quietly embedding
				// against RAG_EMBED_BASEURL (BYO, unmetered). Surface it once per
				// community so the misconfig isn't invisible.
				if on, authd := community.PlatformAI(s, cfg); on && authd && !r.Platform {
					if _, dup := warnedPlatformGap.LoadOrStore(communityID, struct{}{}); !dup {
						log.Warn("rag: community opted into platform AI but PLATFORM_AI_RAG_BASEURL is unset; embedding against RAG_EMBED_BASEURL (BYO, unmetered)",
							"community", communityID, "embed_baseurl", r.EmbedBaseURL)
					}
				}
				key := r.EmbedBaseURL + "|" + r.EmbedModel + "|" + strconv.Itoa(r.EmbedDim)
				var emb rag.Embedder
				if v, ok := embCache.Load(key); ok {
					emb = v.(rag.Embedder)
				} else {
					e := rag.NewOllamaEmbedder(r.EmbedBaseURL, r.EmbedModel, r.EmbedDim)
					// Tenant-supplied Ollama host: reject internal/metadata addresses
					// at dial time (rebinding-safe) on top of the save-time guard.
					e.HTTP = netguard.GuardedClient(2 * time.Minute)
					embCache.Store(key, e)
					emb = e
				}
				// Platform compute (operator pays): meter this community's embeds.
				// The bare embedder is cached by (host|model|dim) and shared across
				// platform communities, so the per-community metering wrapper is
				// applied here, after the cache, not stored in it.
				if r.Platform {
					return rag.NewMeteredEmbedder(emb, usageRec, communityID), nil
				}
				return emb, nil
			}
		}
		log.Info("rag enabled", "backend", ragBackend, "model", cfg.RAGEmbedModel, "store", cfg.RAGStorePath)
		// Per-community admin "Reindex" button. Guard the assignment so the
		// interface field stays nil (not a typed-nil) when RAG is off.
		adminHandler.RAG = ragSvc
		// Community delete drops the tenant's vector collection. Guard so the
		// interface field stays a real nil when RAG is off.
		provSvc.Vectors = ragSvc
	}

	// ----- Agent (per-community AI chat) -----------------------------------
	agentRepo := agent.NewRepo(db)
	agentBus := agent.NewBus()
	agentRunner := agent.NewRunner(agentRepo, agentBus, nc, log)
	agentSvc := agent.NewService(agentRepo)
	// Platform-AI compute routing (SaaS): an opted-in + authorized community runs
	// its agents on the operator's hosted model (metered into ai_usage_events),
	// else the agent's own BYO provider (bare). The summarizer routes to the
	// platform VISION model so a /summary that includes channel images is
	// understood. Shared by the streaming pane (Runner) and synchronous /summary
	// (Service). On any Settings lookup miss it falls back to BYO.
	resolveCompute := func(ctx context.Context, communityID string, a agent.Agent) (agent.Provider, agent.Agent, error) {
		if cfg.SAAS {
			if s, err := cRepo.Settings(ctx, communityID); err == nil {
				wantsVision := a.Vision || a.IsSummarizer
				if ea := community.ResolveAgent(s, cfg, wantsVision); ea.Platform {
					a.Provider, a.BaseURL, a.Model = ea.Provider, ea.BaseURL, ea.Model
					p, perr := agent.NewProvider(a)
					if perr != nil {
						return nil, a, perr
					}
					return agent.NewMeteredProvider(p, usageRec, communityID, ""), a, nil
				}
			}
		}
		p, err := agent.NewProvider(a)
		return p, a, err
	}
	agentRunner.Resolve = resolveCompute
	agentSvc.Resolve = resolveCompute
	agentHandler := &agent.Handler{
		Repo:          agentRepo,
		Svc:           agentSvc,
		Runner:        agentRunner,
		Bus:           agentBus,
		NATS:          nc,
		Uploads:       uploadStore,
		Log:           log,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
	}
	if cfg.AIEnabled {
		// Tools: a tools-enabled agent gets the built-in internal full-text search
		// (search_fts) plus this community's enabled MCP servers. The manager is
		// wired here (the agent package depends only on its ToolSet interface).
		mcpMgr := mcpx.New(
			agentRepo.SearchContent,
			func(ctx context.Context, communityID string) []mcpx.ServerConfig {
				servers, err := agentRepo.ListEnabledMCPServers(ctx, communityID)
				if err != nil {
					return nil
				}
				out := make([]mcpx.ServerConfig, 0, len(servers))
				for _, s := range servers {
					out = append(out, mcpx.ServerConfig{
						Name: s.Name, Transport: s.Transport, Command: s.Command,
						Args: s.Args, URL: s.URL, Headers: s.Headers, Env: s.Env,
					})
				}
				return out
			},
			cfg.AgentMCPAllowStdio,
			log,
		)
		// Extra internal tools that read the local DB to load context. Each query
		// is community-scoped (the WHERE p.community_id binds the agent's own
		// community — that scoping IS the authorization), so the tool can never
		// reach another community's rows. Add more tools by following this shape.
		if cfg.ProjectsEnabled {
			mcpMgr.ListIssues = func(ctx context.Context, communityID string, limit int) []mcpx.IssueRef {
				if limit <= 0 || limit > 200 {
					limit = 50
				}
				rows, err := db.QueryContext(ctx, `
					SELECT i.id, i.title, i.status, p.title
					FROM project_issues i JOIN projects p ON p.id = i.project_id
					WHERE p.community_id = ? AND p.archived_at IS NULL
					ORDER BY i.updated_at DESC LIMIT ?`, communityID, limit)
				if err != nil {
					return nil
				}
				defer rows.Close()
				var out []mcpx.IssueRef
				for rows.Next() {
					var r mcpx.IssueRef
					if rows.Scan(&r.ID, &r.Title, &r.Status, &r.Project) == nil {
						out = append(out, r)
					}
				}
				return out
			}
			mcpMgr.GetIssue = func(ctx context.Context, communityID, id string) (mcpx.IssueDetail, bool) {
				var d mcpx.IssueDetail
				err := db.QueryRowContext(ctx, `
					SELECT i.title, i.body_md, i.status, p.title
					FROM project_issues i JOIN projects p ON p.id = i.project_id
					WHERE i.id = ? AND p.community_id = ?`, id, communityID).
					Scan(&d.Title, &d.Body, &d.Status, &d.Project)
				if err != nil {
					return mcpx.IssueDetail{}, false
				}
				return d, true
			}
		}
		// Semantic search tool — only when RAG is enabled. Maps rag.Hit →
		// agent.SearchHit so mcpx stays independent of internal/rag. Community
		// id is bound here (the agent's own community), never a model argument.
		if ragSvc != nil {
			mcpMgr.RAGSearch = func(ctx context.Context, communityID, query string, limit int) ([]agent.SearchHit, error) {
				if limit <= 0 {
					limit = cfg.RAGSearchDefault
				}
				hits, err := ragSvc.Search(ctx, communityID, query, limit)
				if err != nil {
					return nil, err
				}
				out := make([]agent.SearchHit, 0, len(hits))
				for _, h := range hits {
					out = append(out, agent.SearchHit{
						Kind: h.Kind, RefID: h.RefID, Title: h.Title,
						Snippet: h.Snippet, CreatedAt: h.CreatedAt,
					})
				}
				return out, nil
			}
		}
		agentRunner.Tools = mcpMgr.Build
		// Share-to-channel: copy an assistant answer into a chat channel as the
		// requesting member. Closures (not a chat import in the agent package)
		// keep the dependency one-way and reuse chat's send + fan-out.
		agentHandler.ListChannels = func(ctx context.Context, communityID string) []webtempl.ChannelView {
			chans, err := chatRepo.ListChannels(ctx, communityID, false)
			if err != nil {
				return nil
			}
			out := make([]webtempl.ChannelView, 0, len(chans))
			for _, c := range chans {
				out = append(out, webtempl.ChannelView{ID: c.ID, Slug: c.Slug, Name: c.Name, Topic: c.Topic, IsDefault: c.IsDefault})
			}
			return out
		}
		agentHandler.ShareToChannel = func(ctx context.Context, communityID, channelSlug, authorID, authorName, bodyMD string) (string, error) {
			ch, err := chatRepo.ChannelBySlug(ctx, communityID, channelSlug)
			if err != nil {
				return "", err
			}
			if _, err := chatSvc.Send(ctx, chat.SendInput{
				CommunityID:  communityID,
				ChannelID:    ch.ID,
				AuthorID:     authorID,
				BodyMarkdown: bodyMD,
			}); err != nil {
				return "", err
			}
			chatBus.Broadcast(ch.ID)
			chatNewMsgBus.Broadcast(ch.ID)
			if nc != nil && nc.IsConnected() {
				_ = nc.Publish(natsx.ChatSubject(communityID), []byte(ch.ID))
				_ = nc.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
			}
			// Relay to outbound webhooks — this is real member content landing in
			// the channel, but it bypasses chat.PostSend (where the normal relay
			// lives), so fire it here. No-op when webhooks are off.
			if chatHandler.RelayOut != nil {
				chatHandler.RelayOut(communityID, ch.ID, authorName, bodyMD, ch.Name, "", "", nil)
			}
			return ch.Name, nil
		}
		// $-reference autocomplete: non-agent results — forum subjects plus
		// projects / issues / discussions (when Projects is on). Agent-thread
		// results come straight from agent.Repo. Closure avoids importing
		// forum/projects into the agent package.
		agentHandler.SearchExternalRefs = func(ctx context.Context, communityID, q string, limit int) []webtempl.AgentRefView {
			var out []webtempl.AgentRefView
			if threads, err := forumRepo.ListThreadsFiltered(ctx, communityID, "", q, limit); err == nil {
				for _, t := range threads {
					out = append(out, webtempl.AgentRefView{Kind: "forum", ID: t.ID, Title: t.Subject})
				}
			}
			if cfg.ProjectsEnabled {
				for _, ref := range projectsRepo.SearchRefs(ctx, communityID, q, limit) {
					out = append(out, webtempl.AgentRefView{Kind: ref.Kind, ID: ref.ID, Title: ref.Title})
				}
			}
			return out
		}
		// Resolve a $-referenced thread to its text so the agent gets the
		// content instead of the literal "$Title" token. Agent threads → full
		// conversation; forum threads → opening post + replies. (Project kinds
		// fall back to a title marker in the handler.)
		agentHandler.ResolveRef = func(ctx context.Context, communityID, kind, id string) (string, string, bool) {
			switch kind {
			case "agent":
				th, err := agentRepo.ThreadByID(ctx, id)
				if err != nil || th.CommunityID != communityID {
					return "", "", false
				}
				msgs, _ := agentRepo.Messages(ctx, id)
				var b strings.Builder
				for _, m := range msgs {
					if m.Role == agent.RoleSystem || strings.TrimSpace(m.BodyMD) == "" {
						continue
					}
					role := "User"
					if m.Role == agent.RoleAssistant {
						role = "Assistant"
					}
					b.WriteString(role + ": " + m.BodyMD + "\n")
				}
				return th.Title, capRefContent(b.String()), true
			case "forum":
				th, err := forumRepo.GetThread(ctx, id)
				if err != nil || th.CommunityID != communityID {
					return "", "", false
				}
				var b strings.Builder
				b.WriteString(th.AuthorName + ": " + th.BodyMarkdown + "\n")
				if posts, err := forumRepo.ListPosts(ctx, id); err == nil {
					for _, p := range posts {
						if p.DeletedAt != nil || strings.TrimSpace(p.BodyMarkdown) == "" {
							continue
						}
						b.WriteString(p.AuthorName + ": " + p.BodyMarkdown + "\n")
					}
				}
				return th.Subject, capRefContent(b.String()), true
			default:
				return "", "", false
			}
		}
		// Chat-agents: surface the community's in-chat agents in the roster
		// (always-online bot rows) and in the @mention autocomplete, so a member
		// can see and address a bot by name. Closures avoid a chat↔agent cycle.
		presenceHandler.Agents = func(ctx context.Context, communityID string) ([]presence.ChatAgent, error) {
			ags, err := agentRepo.ListInChatAgents(ctx, communityID)
			if err != nil {
				return nil, err
			}
			out := make([]presence.ChatAgent, 0, len(ags))
			for _, a := range ags {
				out = append(out, presence.ChatAgent{ID: a.ID, DisplayName: a.Name, AvatarURL: a.AvatarURL})
			}
			return out, nil
		}
		chatHandler.MentionAgents = func(ctx context.Context, communityID string) []webtempl.MentionHit {
			ags, err := agentRepo.ListInChatAgents(ctx, communityID)
			if err != nil {
				return nil
			}
			out := make([]webtempl.MentionHit, 0, len(ags))
			for _, a := range ags {
				out = append(out, webtempl.MentionHit{UserID: a.ID, DisplayName: a.Name})
			}
			return out
		}
		// Trigger dispatch: a user message that @mentions or prefix-summons a
		// bound agent opens a forum thread (forum.CreateAgentThread) and streams
		// the agent's reply into it (chatagents.ThreadRunner). chatagents is the
		// seam over chat + agent + forum; the loop guard lives in Dispatch
		// (user-kind only).
		threadRunner := chatagents.NewThreadRunner(forumRepo, forumBus, nc, 0, log)
		threadRunner.Tools = mcpMgr.Build     // same internal-search + MCP tools as the agent pane
		threadRunner.Resolve = resolveCompute // same platform-compute routing + metering as the pane
		// Per-community agent prompt rate limiter, shared by both trigger
		// surfaces (chat send + agent-thread reply). Limits come from the
		// community row; super-admins bypass inside the Gate.
		agentGate := agentlimit.NewGate(func(ctx context.Context, communityID string) (agentlimit.Limits, error) {
			c, err := cRepo.ByID(ctx, communityID)
			if err != nil {
				return agentlimit.Limits{}, err
			}
			return agentlimit.Limits{PerUserMin: c.AgentRatePerUserMin, PerCommunityMin: c.AgentRatePerCommunityMin}, nil
		})
		dispatcher := chatagents.NewDispatcher(agentRepo, forumHandler.CreateAgentThread, threadRunner, agentGate, log)
		chatHandler.Dispatch = func(ctx context.Context, t chat.AgentTrigger) chat.DispatchResult {
			res := dispatcher.Dispatch(ctx, chatagents.Trigger{
				CommunityID: t.CommunityID, Slug: t.Slug, ChannelID: t.ChannelID,
				AuthorID: t.AuthorID, AuthorName: t.AuthorName, Body: t.Body, Kind: t.Kind,
				IsSuperAdmin: t.IsSuperAdmin,
			})
			return chat.DispatchResult{RateLimited: res.RateLimited, RetryAfter: res.RetryAfter}
		}
		// Reply-as-prompt: a human reply in an agent thread re-runs the agent
		// over the full thread history. forum stays agent-free — it hands us the
		// thread's agent_id (plus who replied) and we gate + load + run it.
		forumHandler.OnAgentReply = func(ctx context.Context, communityID, threadID, agentID, userID string, isSuperAdmin bool) forum.AgentReplyResult {
			if dec := agentGate.Check(ctx, communityID, userID, isSuperAdmin); !dec.Allowed {
				log.Info("chatagents: thread reply rate limited", "community", communityID, "user", userID, "retry", dec.RetryAfter)
				return forum.AgentReplyResult{RateLimited: true, RetryAfter: dec.RetryAfter}
			}
			a, err := agentRepo.AgentByID(ctx, agentID)
			if err != nil {
				log.Warn("chatagents: reply agent lookup", "agent", agentID, "err", err)
				return forum.AgentReplyResult{}
			}
			threadRunner.Generate(communityID, threadID, a)
			return forum.AgentReplyResult{}
		}
		// Admin form's channel picker + live roster refresh after a save.
		agentHandler.RosterBump = presenceTracker.Bump
	}
	webtempl.AIEnabled = cfg.AIEnabled
	webtempl.RAGEnabled = cfg.RAGEnabled

	// Time accounting: per-community budget + global personal timer/journal.
	timebudgetHandler := &timebudget.Handler{Repo: timebudget.NewRepo(db), Log: log}
	// Optional "tag a task" project <select> in the budget entry form.
	// Closure (not a direct import) to avoid a projects → timebudget cycle,
	// same trick as chat.ListProjects above. Nil when Projects is disabled.
	timebudgetHandler.ListProjects = func(ctx context.Context, communityID string) []webtempl.TimeProjectView {
		if !cfg.ProjectsEnabled {
			return nil
		}
		rows, err := projectsRepo.ListActiveForCommunity(ctx, communityID)
		if err != nil {
			log.Warn("budget: list projects", "err", err)
			return nil
		}
		out := make([]webtempl.TimeProjectView, 0, len(rows))
		for _, p := range rows {
			out = append(out, webtempl.TimeProjectView{ID: p.ID, Title: p.Title})
		}
		return out
	}
	worklogHandler := &worklog.Handler{Repo: worklog.NewRepo(db), Log: log}
	webtempl.TimeEnabled = cfg.TimeEnabled

	invitesHandler := &invites.Handler{AuthRepo: aRepo, Chat: chatHandler, Sessions: sessions, Log: log}

	// ----- Pastes (per-community pastebin) ----------------------------------
	pastesRepo := pastes.NewRepo(db)
	pastesHandler := &pastes.Handler{
		Svc:           pastes.NewService(pastesRepo),
		Repo:          pastesRepo,
		ChatRepo:      chatRepo,
		BaseURL:       cfg.BaseURL,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}
	// PostToChat posts a saved paste's URL into a channel as the member and fans
	// it out (Bus + NATS + relay) — same shape as the agent share-to-channel
	// closure (above). A closure keeps the pastes package from importing chat's
	// broadcast wiring.
	pastesHandler.PostToChat = func(ctx context.Context, communityID, channelID, authorID, bodyMD string) error {
		if _, err := chatSvc.Send(ctx, chat.SendInput{
			CommunityID:  communityID,
			ChannelID:    channelID,
			AuthorID:     authorID,
			BodyMarkdown: bodyMD,
		}); err != nil {
			return err
		}
		chatBus.Broadcast(channelID)
		chatNewMsgBus.Broadcast(channelID)
		if nc != nil && nc.IsConnected() {
			_ = nc.Publish(natsx.ChatSubject(communityID), []byte(channelID))
			_ = nc.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
		}
		if chatHandler.RelayOut != nil {
			if ch, err := chatRepo.ChannelByID(ctx, channelID); err == nil {
				chatHandler.RelayOut(communityID, channelID, "", bodyMD, ch.Name, "", "", nil)
			}
		}
		return nil
	}
	// /paste slash command → create a draft paste, return its id so chat
	// redirects the author to the editor.
	chatHandler.NewPaste = func(ctx context.Context, communityID, channelID, authorID string) string {
		p, err := pastesHandler.Svc.CreateDraft(ctx, communityID, channelID, authorID)
		if err != nil {
			log.Error("paste: create draft", "err", err)
			return ""
		}
		return p.ID
	}

	// ----- Notes (per-community shared notes) -------------------------------
	notesRepo := notes.NewRepo(db)
	notesHandler := &notes.Handler{
		Svc:           notes.NewService(notesRepo),
		Repo:          notesRepo,
		ChatRepo:      chatRepo,
		BaseURL:       cfg.BaseURL,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}
	// PostToChat drops a shared note's URL into a channel as the member and fans
	// it out — identical shape to the pastes closure above (keeps notes free of a
	// chat import cycle).
	notesHandler.PostToChat = func(ctx context.Context, communityID, channelID, authorID, bodyMD string) error {
		if _, err := chatSvc.Send(ctx, chat.SendInput{
			CommunityID:  communityID,
			ChannelID:    channelID,
			AuthorID:     authorID,
			BodyMarkdown: bodyMD,
		}); err != nil {
			return err
		}
		chatBus.Broadcast(channelID)
		chatNewMsgBus.Broadcast(channelID)
		if nc != nil && nc.IsConnected() {
			_ = nc.Publish(natsx.ChatSubject(communityID), []byte(channelID))
			_ = nc.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
		}
		if chatHandler.RelayOut != nil {
			if ch, err := chatRepo.ChannelByID(ctx, channelID); err == nil {
				chatHandler.RelayOut(communityID, channelID, "", bodyMD, ch.Name, "", "", nil)
			}
		}
		return nil
	}
	// LookupCommunity resolves (name, slug) for the public token reader, which
	// carries no slug in its URL.
	notesHandler.LookupCommunity = func(ctx context.Context, id string) (string, string, bool) {
		c, err := cRepo.ByID(ctx, id)
		if err != nil {
			return "", "", false
		}
		return c.Name, c.Slug, true
	}

	// ----- Web Push (VAPID) -------------------------------------------------
	vapidPub, vapidPriv, err := push.LoadOrCreateVAPID(cfg.VAPIDPublic, cfg.VAPIDPrivate, cfg.VAPIDKeysFile, log)
	if err != nil {
		log.Warn("push: VAPID load/generate failed — push disabled", "err", err)
	}
	pushRepo := push.NewRepo(db)
	pushSender := &push.Sender{
		Repo:    pushRepo,
		Public:  vapidPub,
		Private: vapidPriv,
		Subject: cfg.VAPIDSubject,
		Log:     log,
	}
	pushHandler := &push.Handler{
		Repo:      pushRepo,
		Sender:    pushSender,
		PublicKey: vapidPub,
		AuthSvc:   svc,
		AuthRepo:  aRepo,
		Log:       log,
	}
	pushHandler.Mount(r)
	// Digest worker: scans push_pending every 30s, batches buffered
	// events into one consolidated notification per (user, community)
	// when their interval has elapsed.
	digestCtx, cancelDigest := context.WithCancel(context.Background())
	defer cancelDigest()
	(&push.DigestWorker{Repo: pushRepo, Sender: pushSender, Log: log}).Start(digestCtx)
	// ----- Uploads orphan sweep -------------------------------------------
	// Runs every hour, deletes upload rows older than 24h with no
	// reference anywhere (chat attachments, project Docs, issue
	// attachments, or markdown body). Trims the storage footprint
	// of "user picked a file then closed the tab" cases.
	(&uploads.SweepWorker{Store: uploadStore, Log: log}).Start(digestCtx)
	// ----- Data-export queue + expiry sweep -------------------------------
	// Drains pending owner-requested exports (builds one ZIP at a time) and
	// deletes artifacts past their 7-day TTL (a new request is then required).
	(&dataexport.Worker{Svc: exportSvc, Log: log}).Start(digestCtx)
	// ----- RAG embed-outbox drain -----------------------------------------
	// Drains embed_outbox (written by migration 00039 triggers), embeds each
	// changed row via Ollama, and upserts the vector store. This is RAG's
	// realtime catchup — eventually-consistent, bounded by RAG_WORKER_INTERVAL.
	if ragSvc != nil {
		(&rag.Worker{
			Repo:     rag.NewRepo(db),
			Svc:      ragSvc,
			Interval: time.Duration(cfg.RAGWorkerSeconds) * time.Second,
			Batch:    cfg.RAGWorkerBatch,
			Log:      log,
		}).Start(digestCtx)
	}

	// ----- Mailbox (IMAP ingest) -------------------------------------------
	// Single shared read-only IMAP account → per-community filter routing
	// into /inbox. The feature flag toggles the route, the topbar link,
	// and the poll worker. DB tables exist regardless so toggling the
	// flag never needs a schema migration. EnsureAccount writes the
	// singleton account row; PollWorker reads envelopes (no DB writes
	// until Phase 3 when filter matching lands).
	var mailboxHandler *mailbox.Handler
	var mailboxBus *mailbox.Bus
	if cfg.MailboxEnabled {
		mailboxRepo := mailbox.NewRepo(db)
		mailboxBus = mailbox.NewBus()
		mailboxAccCfg := mailbox.AccountConfig{
			Host:     cfg.MailboxHost,
			Port:     cfg.MailboxPort,
			Username: cfg.MailboxUser,
			Password: cfg.MailboxPass,
			TLSMode:  cfg.MailboxTLS,
		}
		mailboxSvc := mailbox.NewService(mailboxRepo, mailboxAccCfg, projectsSvc, projectsRepo, aRepo, cfg.MailboxSystemUserID)
		projectsHandler.RefetchEmailFn = func(ctx context.Context, issueID string) (bool, int, error) {
			res, err := mailboxSvc.RefetchIssueFromEmail(ctx, issueID)
			return res.BodyUpdated, res.AttachmentsAdded, err
		}
		mailboxHandler = &mailbox.Handler{
			Repo:          mailboxRepo,
			AuthRepo:      aRepo,
			CommunityRepo: cRepo,
			Svc:           mailboxSvc,
			Bus:           mailboxBus,
			NATS:          nc,
			Log:           log,
		}
		if cfg.MailboxHost != "" && cfg.MailboxUser != "" {
			acc, err := mailboxRepo.EnsureAccount(ctx, mailboxAccCfg)
			if err != nil {
				log.Warn("mailbox: EnsureAccount failed", "err", err)
			} else {
				if cfg.MailboxRescanOnBoot {
					if n, err := mailboxRepo.ResetAllFolderCursors(ctx, acc.ID); err != nil {
						log.Warn("mailbox: rescan-on-boot reset failed", "err", err)
					} else {
						log.Info("mailbox: rescan-on-boot — folder cursors reset", "folders", n)
					}
				}
				(&mailbox.PollWorker{
					Cfg:       mailboxAccCfg,
					AccountID: acc.ID,
					Interval:  cfg.MailboxPollInterval,
					Repo:      mailboxRepo,
					Svc:       mailboxSvc,
					Bus:       mailboxBus,
					NATS:      nc,
					Log:       log,
				}).Start(digestCtx)
			}
		}
	}
	webtempl.MailboxEnabled = cfg.MailboxEnabled
	// Wire the sender so other packages (chat, forum, projects) can call
	// notify helpers without importing each other. Each package owns the
	// "what counts as a notifiable event" logic.
	pushNotifyFn := func(ctx context.Context, communityID, kind string, userIDs []string, title, body, url string) {
		n := push.Notification{Title: title, Body: body, URL: url, Tag: kind}
		if len(userIDs) > 0 {
			pushSender.SendToUsers(ctx, communityID, kind, userIDs, n)
			return
		}
		pushSender.SendToCommunity(ctx, communityID, kind, n)
	}
	chatHandler.PushNotify = pushNotifyFn
	forumHandler.PushNotify = pushNotifyFn
	projectsHandler.PushNotify = pushNotifyFn

	// ----- Webhooks (inbound bot messages + outbound chat relay) -----------
	var webhooksHandler *webhooks.Handler
	if cfg.WebhooksEnabled {
		whRepo := webhooks.NewRepo(db)
		whSvc := webhooks.NewService(whRepo)
		whSvc.BlockOutbound = cfg.SAAS // SSRF guard on tenant-supplied target URLs
		webhooksHandler = &webhooks.Handler{
			Repo:          whRepo,
			Svc:           whSvc,
			Chat:          chatSvc,
			ChatRepo:      chatRepo,
			ChatBus:       chatBus,
			ChatNewMsgBus: chatNewMsgBus,
			NATS:          nc,
			BaseURL:       cfg.BaseURL,
			MaxBytes:      cfg.WebhooksMaxBytes,
			Log:           log,
			Uploads:       uploadStore,
			Debug:         debugRec,
		}
		relay := webhooks.NewRelay(whRepo, log)
		relay.Debug = debugRec
		if cfg.SAAS {
			// Untrusted tenant targets: reject internal/metadata addresses at
			// dial time (rebinding-safe) and re-validate every redirect hop.
			relay.Client = netguard.GuardedClient(10 * time.Second)
		}
		// Resolve a message's upload IDs into fetchable outbound attachments:
		// a shared-signed, session-less URL (served by uploads.GetFile) plus
		// MIME + filename. Lets a generic-webhook consumer download images.
		relay.ResolveAttachments = func(ctx context.Context, uploadIDs []string) []webhooks.OutboundAttachment {
			out := make([]webhooks.OutboundAttachment, 0, len(uploadIDs))
			for _, id := range uploadIDs {
				u, err := uploadStore.Get(ctx, id)
				if err != nil {
					continue
				}
				out = append(out, webhooks.OutboundAttachment{
					URL:  cfg.BaseURL + uploadStore.SignedURL(id, "", 24*time.Hour),
					MIME: u.MIME,
					Name: u.Filename,
				})
			}
			return out
		}
		chatHandler.RelayOut = relay.DispatchChat
		// Forum new-thread announcements land in #general — relay them too so
		// external chat mirrors hear about new threads. Same Dispatch as chat.
		forumHandler.RelayOut = relay.Dispatch
		// Forum-thread content (new-thread announce + human replies) also relays
		// with the thread identity attached, so a bridge can mirror it into one
		// external thread (e.g. a Matrix m.thread). See webhooks.OutboundMsg.
		forumHandler.RelayThread = func(cid, chID, chName, author, body, threadID, postID, subject string, root bool) {
			relay.DispatchForum(webhooks.OutboundMsg{
				CommunityID: cid, ChannelID: chID, ChannelName: chName,
				Author: author, BodyMD: body,
				ThreadID: threadID, MessageID: postID, Subject: subject, ThreadRoot: root,
			})
		}
		// Inbound forum routing: a generic message carrying a thread_key opens or
		// appends to a forum thread instead of posting in the chat channel. The
		// forum-side write goes through forum.Service (closures, no import cycle).
		webhooksHandler.OpenForumThread = func(ctx context.Context, communityID, author, subject, markdown string) (string, error) {
			// Resolve the slug for the chat thread_announce deep link; the
			// inbound /hooks route carries no community in context.
			slug := ""
			if c, err := cRepo.ByID(ctx, communityID); err == nil {
				slug = c.Slug
			}
			return forumHandler.OpenWebhookThread(ctx, communityID, slug, author, subject, markdown)
		}
		webhooksHandler.AddForumPost = func(ctx context.Context, threadID, author, avatar, markdown string) (string, error) {
			p, err := forumHandler.Svc.CreateWebhookPost(ctx, threadID, author, avatar, markdown)
			return p.ID, err
		}
		webhooksHandler.NotifyForumThread = forumHandler.BroadcastThreadID
	}
	webtempl.WebhooksEnabled = cfg.WebhooksEnabled

	// Wire the projects list for the chat extract-to-project modal.
	// Closure to avoid an import cycle (chat package can't depend on
	// projects). Empty slice when projects feature is disabled.
	chatHandler.ListProjects = func(ctx context.Context, communityID string) []webtempl.ChatProjectView {
		if !cfg.ProjectsEnabled {
			return nil
		}
		rows, err := projectsRepo.ListActiveForCommunity(ctx, communityID)
		if err != nil {
			log.Warn("chat extract: list projects", "err", err)
			return nil
		}
		out := make([]webtempl.ChatProjectView, 0, len(rows))
		for _, p := range rows {
			out = append(out, webtempl.ChatProjectView{ID: p.ID, Name: p.Title})
		}
		return out
	}

	// Wire the /summary chat slash command: summarise the channel's last 300
	// messages with an agent (in a public agent thread) and return the recap
	// for the requester's ephemeral panel; PublishSummary posts it back on
	// demand. Closures bridge chat → agent → chat without an import cycle. Only
	// when the Agent feature is on.
	if cfg.AIEnabled {
		// relaySlash mirrors slash-command output (/summary, /prompt results) to
		// any matching outbound webhooks. These are KindSystem messages, so the
		// normal user-send relay path (chat handler) skips them — relay here
		// explicitly so external integrations hear agent answers too. Never
		// passes a KindWebhook body, so no echo loop. No-op when webhooks are off.
		relaySlash := func(ctx context.Context, communityID, channelID, label, body string) {
			if chatHandler.RelayOut == nil {
				return
			}
			chName := ""
			if ch, err := chatRepo.ChannelByID(ctx, channelID); err == nil {
				chName = ch.Name
			}
			chatHandler.RelayOut(communityID, channelID, label, body, chName, "", "", nil)
		}
		chatHandler.Summary = func(ctx context.Context, communityID, channelID, requesterID, requesterName string) chat.SummaryResult {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()

			fail := func(msg string) chat.SummaryResult { return chat.SummaryResult{Err: msg} }

			agents, _ := agentRepo.ListEnabledAgents(ctx, communityID)
			if len(agents) == 0 {
				return fail("No AI agent is enabled — an admin can add one in Admin → AI.")
			}
			// Use the agent the admin marked for summaries; fall back to the
			// first enabled one when none is marked.
			sumAgent := agents[0]
			if a, err := agentRepo.SummarizerAgent(ctx, communityID); err == nil {
				sumAgent = a
			}
			convo := formatChatForSummary(chatRepo, ctx, channelID)
			if strings.TrimSpace(convo) == "" {
				return fail("Nothing to summarise yet.")
			}
			chName := "this channel"
			if ch, err := chatRepo.ChannelByID(ctx, channelID); err == nil {
				chName = "#" + ch.Name
			}
			prompt := "Summarise this chat conversation from " + chName + ". Give a concise recap as short bullet points: key topics, any decisions, and open questions.\n\n" + convo
			// A vision summarizer also gets the channel's recent images so the
			// recap can describe them.
			var images []string
			if sumAgent.Vision {
				images = recentChannelImages(ctx, chatRepo, uploadStore, channelID, 8)
				if len(images) > 0 {
					prompt += "\n\nThe channel's most recent images are attached — fold anything notable in them into the recap."
				}
			}
			threadID, answer, err := agentHandler.Svc.SummarizeToThread(ctx, communityID, requesterID, sumAgent, "Summary of "+chName, prompt, images)
			if err != nil || strings.TrimSpace(answer) == "" {
				log.Warn("summary: generate", "err", err)
				return fail("Couldn't generate a summary right now.")
			}
			bodyHTML, _ := render.RenderMarkdown(answer)
			threadURL := ""
			if c, err := cRepo.ByID(ctx, communityID); err == nil && threadID != "" {
				threadURL = "/c/" + c.Slug + "/agent/" + threadID
			}
			return chat.SummaryResult{ThreadID: threadID, BodyHTML: bodyHTML, ThreadURL: threadURL}
		}
		// PublishSummary posts a generated /summary into the channel as a system
		// ("yellow") message everyone sees. The answer is re-read from its agent
		// thread server-side — after confirming the thread is this community's
		// and belongs to the requester — so the client only supplies the id.
		chatHandler.PublishSummary = func(ctx context.Context, communityID, channelID, threadID, requesterID, requesterName string) bool {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()

			t, err := agentRepo.ThreadByID(ctx, threadID)
			if err != nil || t.CommunityID != communityID || t.UserID != requesterID {
				return false
			}
			msgs, err := agentRepo.Messages(ctx, threadID)
			if err != nil {
				return false
			}
			answer := ""
			for _, m := range msgs {
				if m.Role == agent.RoleAssistant && m.Status == agent.StatusDone && strings.TrimSpace(m.BodyMD) != "" {
					answer = strings.TrimSpace(m.BodyMD)
				}
			}
			if answer == "" {
				return false
			}
			// Build the header (with the thread link) FIRST, then the answer.
			// The link must precede the answer: an LLM reply can end with a
			// dangling/unclosed code fence that would otherwise swallow a
			// trailing link and render it as a literal URL.
			header := "🤖 **Channel summary** _(shared by " + requesterName + ")_"
			if c, err := cRepo.ByID(ctx, communityID); err == nil {
				base := strings.TrimRight(cfg.BaseURL, "/")
				header += " · [↗ View in Agent thread](" + base + "/c/" + c.Slug + "/agent/" + threadID + ")"
			}
			body := header + "\n\n" + answer
			if _, err := chatSvc.PostSystemMarkdown(ctx, communityID, channelID, body); err != nil {
				log.Warn("summary: publish", "err", err)
				return false
			}
			chatBus.Broadcast(channelID)
			chatNewMsgBus.Broadcast(channelID)
			if nc != nil && nc.IsConnected() {
				_ = nc.Publish(natsx.ChatSubject(communityID), []byte(channelID))
				_ = nc.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
			}
			relaySlash(ctx, communityID, channelID, "/summary", body)
			return true
		}
		// /prompt <text> — run a free-form prompt through an agent in a new
		// public thread and post the answer + link back to the channel.
		chatHandler.Prompt = func(ctx context.Context, communityID, channelID, requesterID, requesterName, promptText string) {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			post := func(body string) {
				if _, err := chatSvc.PostSystemMarkdown(ctx, communityID, channelID, body); err != nil {
					log.Warn("prompt: post back", "err", err)
					return
				}
				chatBus.Broadcast(channelID)
				chatNewMsgBus.Broadcast(channelID)
				if nc != nil && nc.IsConnected() {
					_ = nc.Publish(natsx.ChatSubject(communityID), []byte(channelID))
					_ = nc.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
				}
				relaySlash(ctx, communityID, channelID, "/prompt", body)
			}
			agents, _ := agentRepo.ListEnabledAgents(ctx, communityID)
			if len(agents) == 0 {
				post("🤖 No AI agent is enabled — an admin can add one in Admin → AI.")
				return
			}
			a := agents[0]
			// Run through the streaming Runner (not the synchronous summariser)
			// so an open agent-thread tab streams the answer live, then post the
			// result to chat once it finishes.
			t, err := agentHandler.Svc.CreateThread(ctx, communityID, requesterID, a, agent.VisibilityShared)
			if err != nil {
				post("🤖 Couldn't start that prompt right now.")
				return
			}
			asstID, history, err := agentHandler.Svc.Send(ctx, t, requesterID, promptText, nil)
			if err != nil {
				post("🤖 Couldn't start that prompt right now.")
				return
			}
			agentBus.Broadcast(t.ID) // show the user turn + placeholder to thread viewers
			if nc != nil && nc.IsConnected() {
				_ = nc.Publish(natsx.AgentThreadSubject(communityID, t.ID), []byte(t.ID))
			}
			if err := agentHandler.Runner.Start(communityID, t.ID, asstID, a, history); err != nil {
				log.Warn("prompt: start", "err", err)
			}
			// Wait for the streaming generation to finish (the thread streams it
			// live meanwhile), then read the final answer.
			for agentHandler.Runner.IsRunning(t.ID) && ctx.Err() == nil {
				time.Sleep(250 * time.Millisecond)
			}
			answer := ""
			if m, err := agentRepo.MessageByID(ctx, asstID); err == nil {
				answer = strings.TrimSpace(m.BodyMD)
			}
			if answer == "" {
				post("🤖 Couldn't complete that prompt right now.")
				return
			}
			header := "🤖 **Prompt complete** _(requested by " + requesterName + ")_"
			if c, err := cRepo.ByID(ctx, communityID); err == nil {
				header += " · [↗ View in Agent thread](" + strings.TrimRight(cfg.BaseURL, "/") + "/c/" + c.Slug + "/agent/" + t.ID + ")"
			}
			post(header + "\n\n" + answer)
		}
	}

	// ----- Lobbies (guest access) ------------------------------------------
	var lobbiesHandler *lobbies.Handler
	if cfg.GuestAccessEnabled {
		lobbiesRepo := lobbies.NewRepo(db)
		lobbiesSvc := lobbies.NewService(lobbiesRepo, svc)
		lobbiesHandler = &lobbies.Handler{
			Svc:           lobbiesSvc,
			Repo:          lobbiesRepo,
			Bus:           lobbies.NewBus(),
			NATS:          nc,
			Uploads:       uploadStore,
			SessionSecret: cfg.SessionKey,
			PushNotify:    pushNotifyFn,
			Log:           log,
		}
		webtempl.GuestAccessEnabled = true
		// Public guest-side routes — token-authed, no community membership.
		r.Get("/lobby/{token}", lobbiesHandler.GetGuestView)
		r.Get("/lobby/{token}/closed", lobbiesHandler.GetClosed)
		r.Get("/lobby/{token}/stream", lobbiesHandler.GetGuestStream)
		r.Group(func(r chi.Router) {
			r.Use(httprate.LimitByIP(30, time.Minute))
			r.Post("/lobby/{token}/join", lobbiesHandler.PostGuestJoin)
			r.Post("/lobby/{token}/send", lobbiesHandler.PostGuestSend)
			r.Post("/lobby/{token}/upload", lobbiesHandler.PostGuestUpload)
		})
	}

	// Public inbound webhook receiver — token-authed, no session/CSRF, like
	// the guest lobby routes above. Behind a per-IP rate limit; the handler
	// caps the body size.
	if cfg.WebhooksEnabled && webhooksHandler != nil {
		r.Group(func(r chi.Router) {
			r.Use(httprate.LimitByIP(60, time.Minute))
			r.Post("/hooks/{token}", webhooksHandler.PostInbound)
		})
	}

	// Project change → chat digest. Posts one system message per
	// community per tick listing projects with new activity. Disabled
	// when PROJECT_CHAT_DIGEST_MINUTES = 0.
	(&projects.ChatDigestWorker{
		DB:              db,
		ChatRepo:        chatRepo,
		ChatBus:         chatBus,
		BaseURL:         cfg.BaseURL,
		IntervalMinutes: cfg.ProjectChatDigestMinutes,
		Log:             log,
	}).Start(digestCtx)

	// Authenticated but not-yet-approved members: only /, /pending, /logout, /profile.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Get("/pending", authHandler.GetPending)
		r.Get("/profile", authHandler.GetProfile)
		r.Post("/profile", authHandler.PostProfile)
		r.Post("/profile/password", authHandler.PostPassword)
		r.Post("/profile/delete/start", authHandler.PostDeleteStart)
		// Personal worklog timer + journal — global (no community scope),
		// available to any signed-in user. Gated by TIME_ENABLED.
		if cfg.TimeEnabled {
			r.Get("/journal", worklogHandler.GetPage)
			r.Post("/timer/start", worklogHandler.PostStart)
			r.Post("/timer/stop", worklogHandler.PostStop)
			r.Post("/timer/note", worklogHandler.PostNote)
		}
	})

	bookmarksRepo := bookmarks.NewRepo(db)
	bookmarksHandler := &bookmarks.Handler{
		Repo:          bookmarksRepo,
		ChatRepo:      chatRepo,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}

	historyHandler := &history.Handler{
		DB:            db,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}

	// Search — fuses the full-text (search_fts) and semantic (rag) indexes with
	// Reciprocal Rank Fusion. Full-text always works; the semantic side is wired
	// only when RAG is enabled (otherwise the closure stays nil and Search
	// degrades to plain full-text ranking). Maps rag.Hit → search.Hit so the
	// search package never imports rag.
	searchSvc := &search.Service{DB: db}
	if ragSvc != nil {
		searchSvc.Semantic = func(ctx context.Context, communityID, query string, limit int) ([]search.Hit, error) {
			hits, err := ragSvc.Search(ctx, communityID, query, limit)
			if err != nil {
				return nil, err
			}
			out := make([]search.Hit, 0, len(hits))
			for _, h := range hits {
				out = append(out, search.Hit{Kind: h.Kind, RefID: h.RefID, Title: h.Title, Snippet: h.Snippet, CreatedAt: h.CreatedAt})
			}
			return out, nil
		}
	}
	searchHandler := &search.Handler{
		Svc:           searchSvc,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}
	// /search chat slash command — reuses the fused search, rendered as an
	// ephemeral panel for the sender. Closure keeps chat decoupled from search.
	chatHandler.Search = func(ctx context.Context, communityID, slug, query string, limit int) []webtempl.SearchResultView {
		// viewerID "" — the chat /search slash command stays community-public
		// only; private-note author search lives on the /search page.
		results, err := searchSvc.Search(ctx, communityID, "", slug, query, limit)
		if err != nil {
			log.Error("chat /search", "err", err)
			return nil
		}
		return search.Views(results)
	}

	// /translate composer typeahead. Ollama-direct, independent of the
	// per-community AI agents. In SaaS each community resolves its own model +
	// Ollama host (community_settings); self-hosted falls back to the global
	// TRANSLATE_* env. Installed whenever the feature is globally enabled (the
	// kill-switch); the resolver decides per request — an empty model leaves
	// the popup inert.
	if cfg.TranslateEnabled {
		chatHandler.Translate = func(ctx context.Context, text string) ([]string, error) {
			c, ok := community.FromContext(ctx)
			if !ok {
				return nil, nil
			}
			s, err := cRepo.Settings(ctx, c.ID)
			if err != nil {
				return nil, err
			}
			tr := community.ResolveTranslate(s, cfg)
			if !tr.Enabled || tr.Model == "" {
				return nil, nil
			}
			// Platform compute (operator pays): meter, attributing to the
			// requesting member. BYO stays bare.
			if tr.Platform {
				var uid string
				if id, ok := auth.FromContext(ctx); ok {
					uid = id.User.ID
				}
				return agent.MeteredTranslate(ctx, usageRec, c.ID, uid, tr.BaseURL, tr.Model, text)
			}
			return agent.Translate(ctx, tr.BaseURL, tr.Model, text)
		}
		// Capability check the composer reads at page render: only fire the
		// typeahead when this community can actually translate, so a tenant
		// where it's off (the SaaS default) never flashes the popup open.
		chatHandler.TranslateEnabled = func(ctx context.Context) bool {
			c, ok := community.FromContext(ctx)
			if !ok {
				return false
			}
			s, err := cRepo.Settings(ctx, c.ID)
			if err != nil {
				return false
			}
			tr := community.ResolveTranslate(s, cfg)
			return tr.Enabled && tr.Model != ""
		}
	}

	pmRepo := privatemsg.NewRepo(db)
	pmBus := privatemsg.NewBus()
	pmSvc := &privatemsg.Service{Repo: pmRepo, Bus: pmBus, Blocks: aRepo}
	pmHandler := &privatemsg.Handler{
		Svc:       pmSvc,
		Repo:      pmRepo,
		Bus:       pmBus,
		AuthRepo:  aRepo,
		Sessions:  sessions,
		Log:       log,
		SendToken: sendSigner, // /messages/stream patches the token to clients
	}

	roomsRepo := rooms.NewRepo(db)
	roomsBus := rooms.NewBus()
	roomsState := rooms.NewState()
	roomsSvc := rooms.NewService(roomsRepo, roomsBus, roomsState)
	roomsHandler := &rooms.Handler{
		Svc:        roomsSvc,
		Repo:       roomsRepo,
		Bus:        roomsBus,
		State:      roomsState,
		AuthRepo:   aRepo,
		CommRepo:   cRepo,
		Sessions:   sessions,
		Log:        log,
		ChatSvc:    chatSvc,
		ChatRepo:   chatRepo,
		ChatBus:    chatBus,
		Mailer:     mailer,
		IceServers: buildIceServers(cfg),
		ForceRelay: cfg.ForceRelay,
	}
	// Seed the bootstrap community's 8 rooms on boot. Other communities
	// get lazy-seeded on first GET /c/{slug}/rooms.
	if err := roomsRepo.EnsureSeeded(ctx, bootCommunity.ID); err != nil {
		log.Warn("rooms seed bootstrap community failed", "err", err)
	}
	// Ensure the bootstrap community has its undeletable #general chat
	// channel (migration seeds existing communities; this covers a
	// freshly-created bootstrap community).
	if _, err := chatRepo.EnsureDefaultChannel(ctx, bootCommunity.ID); err != nil {
		log.Warn("ensure default chat channel failed", "err", err)
	}

	// Heal any Agent generations orphaned by a previous process exit: an LLM
	// completion can't resume mid-stream, so flip lingering "generating" rows
	// to "interrupted" (partial kept, UI offers Regenerate). See §6/agent.
	if cfg.AIEnabled {
		if n, err := agentRepo.MarkGeneratingInterrupted(ctx); err != nil {
			log.Warn("agent: heal generating rows failed", "err", err)
		} else if n > 0 {
			log.Info("agent: healed interrupted generations", "count", n)
		}
		// Same heal for agent reply posts streaming in forum threads.
		if n, err := forumRepo.MarkBotPostsInterrupted(ctx); err != nil {
			log.Warn("chatagents: heal generating bot posts failed", "err", err)
		} else if n > 0 {
			log.Info("chatagents: healed interrupted bot posts", "count", n)
		}
	}

	// Per-community JOIN landing — LoadCommunity runs so the templ can render
	// the community name, but RequireMember does NOT (this is the path that
	// admits new members). Mounted before the main /c/{slug} group so it
	// doesn't get caught by RequireMember.
	r.Route("/c/{slug}/join", func(r chi.Router) {
		r.Use(community.LoadCommunity(cRepo, cfg))
		r.Get("/", invitesHandler.GetJoin)
		r.Post("/confirm", invitesHandler.PostJoinConfirm)
		r.Post("/set-password", invitesHandler.PostJoinSetPassword)
	})

	// Per-community area: every page, every SSE stream, every POST nests under
	// /c/{slug}. LoadCommunity resolves the slug; RequireMember rebinds the
	// viewer's identity to that community's membership row.
	r.Route("/c/{slug}", func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Use(community.LoadCommunity(cRepo, cfg))
		r.Use(community.RequireMember(aRepo))
		r.Use(auth.RequireApproved)

		// Bare /chat redirects to #general so the URL always carries a
		// channel slug. Channel-agnostic actions (upload, mention search,
		// cross-page events, extract) stay at /chat/*; static segments win
		// over the {channel} wildcard in chi so they're never shadowed.
		r.Get("/chat", chatHandler.GetChatRedirect)
		r.Post("/chat/upload", chatHandler.PostUpload)
		r.Get("/chat/mention", chatHandler.GetMentionSearch)
		r.Get("/chat/translate", chatHandler.GetTranslate)
		r.Get("/chat/events", chatHandler.GetEventsStream)
		r.Post("/chat/extract", projectsHandler.PostExtractFromChat)
		r.Post("/chat/forward", chatHandler.PostForward)
		// Channel management (mod create/rename/topic/archive; admin delete
		// — role enforced inside the handlers).
		r.Post("/chat/channels", chatHandler.PostCreateChannel)
		r.Post("/chat/channels/rename", chatHandler.PostRenameChannel)
		r.Post("/chat/channels/topic", chatHandler.PostSetTopic)
		r.Post("/chat/channels/archive", chatHandler.PostArchiveChannel)
		r.Post("/chat/channels/delete", chatHandler.PostDeleteChannel)
		// Per-channel surfaces.
		r.Get("/chat/{channel}", chatHandler.GetPage)
		r.Get("/chat/{channel}/stream", chatHandler.GetStream)
		r.With(sendSigner.Require()).Post("/chat/{channel}/send", chatHandler.PostSend)
		r.Post("/chat/{channel}/search/publish", chatHandler.PostSearchPublish)
		r.Post("/chat/{channel}/summary/publish", chatHandler.PostSummaryPublish)
		r.Post("/chat/{channel}/read", chatHandler.PostMarkRead)
		r.Post("/block", chatHandler.PostBlock)
		r.Post("/unblock", chatHandler.PostUnblock)
		r.Post("/report", chatHandler.PostReport)

		// Pastes — pastebin: /paste (or the composer button) opens an editor at
		// /pastes/{id}; Save posts the link into chat. Static "new" wins over
		// the {id} wildcard.
		r.Post("/pastes/new", pastesHandler.PostNew)
		r.Get("/pastes/{id}", pastesHandler.GetPage)
		r.Post("/pastes/{id}/save", pastesHandler.PostSave)

		// Notes — community shared notes: index, editor at /notes/{id}, live
		// markdown preview, save, delete. Static "new" wins over the {id}
		// wildcard. Share-to-channel + the public token reader are added with
		// their own routes (the reader mounts as a public route, no approval gate).
		r.Get("/notes", notesHandler.GetIndex)
		r.Post("/notes/new", notesHandler.PostNew)
		r.Get("/notes/{id}", notesHandler.GetPage)
		r.Post("/notes/{id}/save", notesHandler.PostSave)
		r.Post("/notes/{id}/preview", notesHandler.PostPreview)
		r.Post("/notes/{id}/share", notesHandler.PostShare)
		r.Post("/notes/{id}/delete", notesHandler.PostDelete)
		// Inline comments. Static "comments" wins over the {id} wildcard, so the
		// per-comment moderate routes sit under /notes/comments/{cid}/… and the
		// add route under /notes/{id}/comments.
		r.Post("/notes/{id}/comments", notesHandler.PostComment)
		r.Post("/notes/comments/{cid}/resolve", notesHandler.PostResolveComment)
		r.Post("/notes/comments/{cid}/delete", notesHandler.PostDeleteComment)

		// Agent — per-community AI chat with threads + history. Static
		// segments (new) win over the {thread} wildcard in chi. Gated by the
		// global AI_ENABLED kill-switch AND (in SaaS) the community's master
		// toggle: a community with ai_enabled=false 404s its agent routes even
		// though the feature is globally mounted (LoadCommunity stamps the flag).
		if cfg.AIEnabled {
			r.Group(func(r chi.Router) {
				r.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						if !webtempl.CommunityAIEnabled(req.Context()) {
							http.NotFound(w, req)
							return
						}
						next.ServeHTTP(w, req)
					})
				})
				r.Get("/agent", agentHandler.GetIndex)
				r.Post("/agent/new", agentHandler.PostNew)
				r.Get("/agent/refs", agentHandler.GetRefSearch)
				r.Get("/agent/{thread}", agentHandler.GetPage)
				r.Get("/agent/{thread}/stream", agentHandler.GetStream)
				r.Post("/agent/{thread}/send", agentHandler.PostSend)
				r.Post("/agent/{thread}/agent", agentHandler.PostSetAgent)
				r.Post("/agent/{thread}/stop", agentHandler.PostStop)
				r.Post("/agent/{thread}/regenerate", agentHandler.PostRegenerate)
				r.Post("/agent/{thread}/share", agentHandler.PostShareToChannel)
				r.Post("/agent/{thread}/delete", agentHandler.PostDelete)
			})
		}

		r.Get("/presence/stream", presenceHandler.GetStream)

		r.Post("/uploads", uploadHandler.PostUpload)

		r.Get("/forum", forumHandler.GetIndex)
		r.Get("/forum/new", forumHandler.GetNew)
		r.With(sendSigner.Require()).Post("/forum/new", forumHandler.PostNew)
		r.Get("/forum/{id}", forumHandler.GetThread)
		r.Get("/forum/{id}/stream", forumHandler.GetThreadStream)
		r.With(sendSigner.Require()).Post("/forum/{id}/reply", forumHandler.PostReply)
		r.Post("/forum/{id}/delete", forumHandler.PostDeleteThread)
		r.Post("/forum/{id}/resolve", forumHandler.PostResolve)
		r.Post("/forum/{id}/unresolve", forumHandler.PostUnresolve)
		r.Post("/forum/{id}/rename", forumHandler.PostRename)
		r.Post("/forum/post/{id}/delete", forumHandler.PostDeletePost)
		r.Post("/forum/promote-chat", forumHandler.PostPromoteChat)

		r.Get("/bookmarks", bookmarksHandler.GetPage)
		r.Get("/bookmarks/list", bookmarksHandler.GetList)
		r.Post("/bookmarks", bookmarksHandler.PostCreate)
		r.Post("/bookmarks/delete", bookmarksHandler.PostDelete)

		r.Get("/history", historyHandler.GetIndex)

		r.Get("/search", searchHandler.GetIndex)
		r.Get("/search/results", searchHandler.GetResults)

		r.Get("/todos", todosHandler.GetIndex)
		r.Post("/todos", todosHandler.PostCreate)
		r.Post("/todos/{id}/status", todosHandler.PostStatus)
		r.Post("/todos/{id}/delete", todosHandler.PostDelete)

		// Time budget — any approved member can VIEW the page (the client
		// sees how much of the month's budget is left). Write endpoints
		// (set budget / log / delete) live in the mod-gated group below.
		if cfg.TimeEnabled {
			r.Get("/budget", timebudgetHandler.GetPage)
		}

		// Per-community notification settings (Web Push opt-in + toggles).
		pushHandler.MountPerCommunity(r)

		// Projects routes ALL live in their own r.Route block below the
		// big /c/{slug} group — see "Projects feature routes" further
		// down. Mounting them here would conflict with that block's
		// /c/{slug}/projects tree and shadow the index page (chi resolves
		// the more-specific Route first, leaving the empty-path
		// /c/{slug}/projects unmatched -> 404).

		r.Group(func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleMod))
			r.Post("/chat/delete", chatHandler.PostDelete)
			// Time budget writes — mod/admin. Setting the monthly budget is
			// further restricted to admin inside PostSetBudget.
			if cfg.TimeEnabled {
				r.Post("/budget", timebudgetHandler.PostSetBudget)
				r.Post("/budget/entry", timebudgetHandler.PostAddEntry)
				r.Post("/budget/entry/{id}/delete", timebudgetHandler.PostDeleteEntry)
			}
			// Lobbies host area — admin/mod only, gated by env flag.
			if cfg.GuestAccessEnabled && lobbiesHandler != nil {
				r.Get("/lobbies", lobbiesHandler.GetIndex)
				r.Post("/lobbies/new", lobbiesHandler.PostNew)
				r.Get("/lobbies/{id}", lobbiesHandler.GetHostView)
				r.Get("/lobbies/{id}/stream", lobbiesHandler.GetHostStream)
				r.Post("/lobbies/{id}/send", lobbiesHandler.PostHostSend)
				r.Post("/lobbies/{id}/close", lobbiesHandler.PostClose)
				r.Post("/lobbies/{id}/archive", lobbiesHandler.PostArchive)
				r.Post("/lobbies/{id}/reopen", lobbiesHandler.PostReopen)
				r.Post("/lobbies/{id}/promote", lobbiesHandler.PostPromote)
				r.Post("/lobbies/{id}/update", lobbiesHandler.PostUpdateGuest)
				r.Post("/lobbies/{id}/delete", lobbiesHandler.PostDelete)
			}
		})

		// Per-community admin area.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleAdmin))
			r.Get("/admin", adminHandler.GetIndex)
			r.Post("/admin/approve", adminHandler.PostApprove)
			r.Post("/admin/reject", adminHandler.PostReject)
			r.Post("/admin/ban", adminHandler.PostBan)
			r.Post("/admin/unban", adminHandler.PostUnban)
			r.Post("/admin/remove", adminHandler.PostRemoveMember)
			r.Post("/admin/set-role", adminHandler.PostSetRole)
			r.Post("/admin/report/resolve", adminHandler.PostResolveReport)
			r.Post("/admin/invite", adminHandler.PostInvite)
			r.Post("/admin/invite/revoke", adminHandler.PostInviteRevoke)
			r.Post("/admin/add-member", adminHandler.PostAddMember)
			r.Post("/admin/toggle-public", adminHandler.PostTogglePublic)
			r.Post("/admin/ai/limits", adminHandler.PostSetAgentLimits)
			r.Post("/admin/reindex", adminHandler.PostReindex)
			r.Post("/forum/{id}/hard-delete", forumHandler.PostHardDeleteThread)
			if cfg.MailboxEnabled && mailboxHandler != nil {
				r.Get("/admin/mail-filters", mailboxHandler.GetCommunityFilters)
				r.Post("/admin/mail-filters", mailboxHandler.PostCommunityFilterCreate)
				r.Post("/admin/mail-filters/{id}/delete", mailboxHandler.PostCommunityFilterDelete)
				r.Post("/admin/mail-filters/{id}/apply", mailboxHandler.PostCommunityFilterApply)
			}
			if cfg.WebhooksEnabled && webhooksHandler != nil {
				r.Get("/admin/webhooks", webhooksHandler.GetAdmin)
				r.Post("/admin/webhooks", webhooksHandler.PostCreate)
				r.Post("/admin/webhooks/toggle", webhooksHandler.PostToggle)
				r.Post("/admin/webhooks/rotate", webhooksHandler.PostRotate)
				r.Post("/admin/webhooks/delete", webhooksHandler.PostDelete)
			}
			if cfg.AIEnabled {
				r.Get("/admin/ai", agentHandler.GetAgents)
				r.Get("/admin/ai/new", agentHandler.GetNewAgentForm)
				r.Post("/admin/ai/mcp", agentHandler.PostSaveMCPServer)
				r.Post("/admin/ai/mcp/{id}/toggle", agentHandler.PostToggleMCPServer)
				r.Post("/admin/ai/mcp/{id}/delete", agentHandler.PostDeleteMCPServer)
				r.Get("/admin/ai/{id}/edit", agentHandler.GetEditAgentForm)
				r.Post("/admin/ai", agentHandler.PostSaveAgent)
				r.Post("/admin/ai/{id}/delete", agentHandler.PostDeleteAgent)
			}
		})

		// Per-community owner Settings (SaaS tenant config). Owner-gated and
		// only mounted in SaaS mode; super-admins pass via the synthetic owner
		// membership. Configures the per-community AI master switch, join
		// policy and translation; RAG + storage cards land with those backends.
		if cfg.SAAS {
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireRole(auth.RoleOwner))
				r.Get("/settings", adminHandler.GetSettings)
				r.Post("/settings", adminHandler.PostSettings)
				r.Post("/settings/migrate-storage", adminHandler.PostMigrateStorage)
				r.Post("/settings/platform-ai/request", adminHandler.PostRequestPlatformAI)
				r.Post("/settings/platform-ai/cancel", adminHandler.PostCancelPlatformAI)
				r.Post("/settings/billing/checkout", adminHandler.PostBillingCheckout)
				r.Post("/settings/delete", adminHandler.PostDeleteCommunity)
				// Owner-initiated data export: live status card (SSE) + request.
				// The download itself is a public token-gated route (see below).
				r.Get("/settings/export/stream", exportHandler.GetStream)
				r.Post("/settings/export", exportHandler.PostRequest)
			})
		}
	})

	// Global admin (uses session's CurrentCommunity membership for the role
	// check) — community creation lives here so it isn't gated by being a
	// member of the new community itself.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Use(auth.RequireRole(auth.RoleAdmin))
		r.Post("/admin/create-community", adminHandler.PostCreateCommunity)
	})

	// Projects feature routes — all under one r.Route("/c/{slug}/projects")
	// block with TWO inner groups:
	//   - Open (auth member OR share-link guest): only LoadCommunity
	//     middleware. callerIdentity inside the handler resolves the
	//     viewer.
	//   - Member-only (auth member + community member): also gets
	//     RequireAuth + RequireMember + RequireApproved.
	// Mounting all projects routes in one tree avoids the chi shadowing
	// gotcha where a separate /c/{slug}/projects route block hides
	// individual /projects/* routes registered inside the broader
	// /c/{slug} group.
	if cfg.ProjectsEnabled {
		r.Route("/c/{slug}/projects", func(r chi.Router) {
			r.Use(community.LoadCommunity(cRepo, cfg))

			// Open — auth member OR share-link guest.
			r.Group(func(r chi.Router) {
				r.Get("/{id}", projectsHandler.GetOverview)
				r.Get("/{id}/todos", projectsHandler.GetTodosTab)
				r.Get("/{id}/docs", projectsHandler.GetDocsTab)
				r.Get("/{id}/comments", projectsHandler.GetCommentsTab)
				r.Get("/{id}/activity", projectsHandler.GetActivityTab)
				r.Get("/{id}/issues", projectsHandler.GetIssuesTab)
				r.Post("/{id}/issues", projectsHandler.PostCreateIssue)
				r.Post("/{id}/issues/close-all", projectsHandler.PostCloseAllIssues)
				r.Get("/{id}/issues/{iid}", projectsHandler.GetIssue)
				r.Post("/{id}/issues/{iid}", projectsHandler.PostIssueEdit)
				r.Post("/{id}/issues/{iid}/delete", projectsHandler.PostIssueDelete)
				r.Post("/{id}/issues/{iid}/move", projectsHandler.PostIssueMove)
				r.Post("/{id}/issues/{iid}/refetch", projectsHandler.PostIssueRefetch)
				r.Post("/{id}/issues/{iid}/comment", projectsHandler.PostIssueComment)
				r.Post("/{id}/issues/{iid}/comment/{cid}", projectsHandler.PostIssueCommentEdit)
				r.Post("/{id}/issues/{iid}/comment/{cid}/delete", projectsHandler.PostIssueCommentDelete)
				r.Post("/{id}/issues/{iid}/attachment", projectsHandler.PostIssueAttachmentUpload)
				r.Post("/{id}/issues/{iid}/attachment/{aid}/delete", projectsHandler.PostIssueAttachmentDelete)
				r.Post("/{id}/issues/{iid}/attachment/{aid}/copy-to-docs", projectsHandler.PostIssueAttachmentCopyToDocs)
				r.Get("/{id}/discussions", projectsHandler.GetDiscussionsTab)
				r.Post("/{id}/discussions", projectsHandler.PostCreateDiscussionThread)
				r.Get("/{id}/discussions/{did}", projectsHandler.GetDiscussionThread)
				r.Post("/{id}/discussions/{did}", projectsHandler.PostEditDiscussionThread)
				r.Post("/{id}/discussions/{did}/delete", projectsHandler.PostDeleteDiscussionThread)
				r.Post("/{id}/discussions/{did}/reply", projectsHandler.PostDiscussionReply)
				r.Post("/{id}/discussions/{did}/reply/{rid}", projectsHandler.PostDiscussionReplyEdit)
				r.Post("/{id}/discussions/{did}/reply/{rid}/delete", projectsHandler.PostDiscussionReplyDelete)

				// Share-to-chat: per-resource POSTs that emit a chat message
				// with a clickable link + the user's optional one-liner.
				// Member-only — guests don't have a chat to write into.
				r.Post("/{id}/share-to-chat", projectsHandler.PostShareProjectToChat)
				r.Post("/{id}/issues/{iid}/share-to-chat", projectsHandler.PostShareIssueToChat)
				r.Post("/{id}/discussions/{did}/share-to-chat", projectsHandler.PostShareDiscussionToChat)
			})

			// Member-only — index, create, edits, lifecycle, share mint,
			// issue status change. Auth + RequireMember + RequireApproved.
			r.Group(func(r chi.Router) {
				r.Use(auth.RequireAuth)
				r.Use(community.RequireMember(aRepo))
				r.Use(auth.RequireApproved)
				r.Get("/", projectsHandler.GetIndex)
				r.Post("/", projectsHandler.PostCreate)
				r.Get("/{id}/stream", projectsHandler.GetStream)
				r.Post("/{id}/title", projectsHandler.PostTitle)
				r.Post("/{id}/desc", projectsHandler.PostDescription)
				r.Post("/{id}/todo", projectsHandler.PostTodoAdd)
				r.Post("/{id}/todo/{tid}", projectsHandler.PostTodoEdit)
				r.Post("/{id}/todo/{tid}/toggle", projectsHandler.PostTodoToggle)
				r.Post("/{id}/todo/{tid}/status", projectsHandler.PostTodoStatus)
				r.Post("/{id}/todo/{tid}/assign", projectsHandler.PostTodoAssign)
				r.Post("/{id}/todo/{tid}/delete", projectsHandler.PostTodoDelete)
				r.Post("/{id}/todo/reorder", projectsHandler.PostTodoReorder)
				r.Post("/{id}/attachment", projectsHandler.PostAttachmentUpload)
				r.Get("/{id}/attachment/{aid}/download", projectsHandler.GetAttachmentDownload)
				r.Post("/{id}/attachment/{aid}/delete", projectsHandler.PostAttachmentDelete)
				r.Post("/{id}/attachment/{aid}/move", projectsHandler.PostAttachmentMove)
				r.Post("/{id}/comment", projectsHandler.PostComment)
				r.Post("/{id}/comment/{cid}", projectsHandler.PostCommentEdit)
				r.Post("/{id}/comment/{cid}/delete", projectsHandler.PostCommentDelete)
				r.Post("/{id}/archive", projectsHandler.PostArchive)
				r.Post("/{id}/unarchive", projectsHandler.PostUnarchive)
				r.Post("/{id}/delete", projectsHandler.PostDeleteProject)
				r.Post("/{id}/share", projectsHandler.PostShareMint)
				r.Post("/{id}/share/revoke", projectsHandler.PostShareRevoke)
				r.Post("/{id}/issues/{iid}/status", projectsHandler.PostIssueStatus)
			})
		})

		// Public guest-landing routes — no community context.
		r.Get("/projects/share/{token}", projectsHandler.GetGuestLanding)
		r.Post("/projects/share/{token}/join", projectsHandler.PostGuestJoin)
		r.Get("/projects/share/{token}/go", projectsHandler.GetGuestBounce)
	}

	// Uploads GET lives at root so stored /uploads/{id}?sig=... URLs survive
	// the multi-community route restructure. The HMAC signature already
	// scopes access (binds upload id + viewer id + exp). Auth.RequireAuth
	// is OFF — the handler resolves auth users AND project-share guest
	// sessions internally so guests can view images on issues.
	r.Get("/uploads/{id}", uploadHandler.GetFile)
	// Public, token-gated data-export download — two-step + crawl-safe. GET
	// renders only a landing page (no payload), so mail scanners / link
	// unfurlers that prefetch the shared link can't pull the data; only a human
	// POST (the form button) streams the ZIP. The id + 32-byte token are the
	// bearer capability (valid until the 7-day expiry); no session required.
	r.Get("/exports/{id}", exportHandler.GetLanding)
	r.Post("/exports/{id}/download", exportHandler.PostDownload)

	// Public, token-gated note reader. /n/<token> renders a note read-only; the
	// 32-byte token is the bearer capability. Identity is optional (Loader is
	// global), so a logged-in member following the link still reads it. Any miss
	// renders the generic "unavailable" page (no existence oracle).
	r.Get("/n/{token}", notesHandler.GetShared)

	// Public Stripe webhook — no session/CSRF; authenticity is the HMAC signature
	// verified in billing.Service.Webhook against STRIPE_WEBHOOK_SECRET. It is the
	// sole authority on subscription state. Mounted only when billing is fully
	// configured.
	if billingSvc.Enabled() {
		r.Post("/billing/webhook", billingSvc.Webhook)
	}

	// Private messages are global — no community membership required.
	// The handler authenticates via the session directly.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		pmHandler.Routes(r)
	})

	// Rooms are community-scoped. The /c/{slug}/rooms tree carries
	// LoadCommunity for everyone; the auth-required slice (grid +
	// invite admin ops) lives in an inner group with RequireAuth +
	// RequireMember, while per-room interaction routes accept either
	// an auth user or an invite-guest (handler.caller() resolves it).
	r.Route("/c/{slug}/rooms", func(r chi.Router) {
		r.Use(community.LoadCommunity(cRepo, cfg))

		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Use(community.RequireMember(aRepo))
			r.Use(auth.RequireApproved)
			roomsHandler.MemberRoutes(r)
		})

		r.Group(func(r chi.Router) {
			roomsHandler.OpenRoutes(r)
		})
	})
	// Guest invite landing stays at the root — anyone with the token can
	// pick a display name and join, regardless of community membership.
	roomsHandler.PublicRoutes(r)

	go roomsState.RunJanitor(ctx, log)

	r.Get("/", dashboardHandler.GetIndex)
	// SaaS self-serve community creation. RequireAuth only (no RequireApproved /
	// RequireMember): a user creating their first community may have no membership
	// yet. The handlers re-check SAAS + the owner-count quota server-side.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Post("/communities/create", dashboardHandler.PostCreate)
		r.Post("/communities/request", dashboardHandler.PostRequest)
	})
	if cfg.MailboxEnabled && mailboxHandler != nil {
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Use(auth.RequireSuperAdmin) // /inbox is the company-owner surface
			r.Get("/inbox", mailboxHandler.GetGlobalInbox)
			r.Get("/inbox/more", mailboxHandler.GetMore)
			r.Get("/inbox/stream", mailboxHandler.GetStream)
			r.Post("/inbox/attach-sender", mailboxHandler.PostAttachSender)
			r.Post("/inbox/attachments/{id}/move", mailboxHandler.PostMoveAttachment)
			r.Post("/inbox/search", mailboxHandler.PostSearch)
		})
	}
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Get("/issues", projectsHandler.GetGlobalIssues)
		r.Get("/issues/stream", projectsHandler.GetGlobalIssuesStream)
	})

	// Hidden support inbox — any signed-in user files a report; reads back
	// only their own. RequireAuth only (no RequireApproved) so even a
	// pending member can reach out for help.
	if supportHandler != nil {
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/report-issue", supportHandler.GetReport)
			r.Post("/report-issue", supportHandler.PostReport)
			r.Get("/report-issue/{iid}", supportHandler.GetReportDetail)
			r.Post("/report-issue/{iid}/reply", supportHandler.PostReply)
		})
		// Super-admin triage view of the whole inbox + status control. Gated
		// by RequireSuperAdmin so it's discoverable without leaking the hidden
		// community into any member-facing surface.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Use(auth.RequireSuperAdmin)
			r.Get("/support-inbox", supportHandler.GetInbox)
			r.Post("/report-issue/{iid}/status", supportHandler.PostStatus)
		})
	}

	// Cross-community readonly chat inbox. ONE engine, two surfaces:
	//   - SaaS member feed (/chats): scoped to the viewer's own communities so a
	//     tenant sees their own chats, not every community's ("no sniffing").
	//   - self-hosted super-admin god-mode (/superadmin/chat): every community.
	// In SaaS the super-admin gets the scoped feed too (god-mode disabled);
	// /superadmin/chat redirects to /chats.
	memberChatInbox := &chat.InboxHandler{Repo: chatRepo, Bus: chatHandler.Bus, NATS: nc, Members: aRepo, GodMode: false, StreamPath: "/chats/stream"}
	godChatInbox := &chat.InboxHandler{Repo: chatRepo, Bus: chatHandler.Bus, NATS: nc, GodMode: true, StreamPath: "/superadmin/chat/stream"}
	if cfg.SAAS {
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/chats", memberChatInbox.GetPage)
			r.Get("/chats/stream", memberChatInbox.GetStream)
		})
	}

	// Platform super-admin surface — global god-mode over every community
	// and user, gated by the SUPERADMIN_EMAILS allowlist.
	superHandler := &superadmin.Handler{AuthRepo: aRepo, Communities: cRepo, Provision: provSvc, Log: log, Bus: chatHandler.Bus, Chat: chatHandler, Debug: debugRec, Usage: usageRec}
	if ragSvc != nil {
		superHandler.RAG = ragSvc
	}
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Use(auth.RequireSuperAdmin)
		r.Get("/superadmin", superHandler.GetIndex)
		if cfg.SAAS {
			// No god-mode all-communities feed in SaaS — the super-admin uses the
			// same scoped /chats as every member.
			r.Get("/superadmin/chat", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "/chats", http.StatusSeeOther)
			})
		} else {
			r.Get("/superadmin/chat", godChatInbox.GetPage)
			r.Get("/superadmin/chat/stream", godChatInbox.GetStream)
		}
		r.Get("/superadmin/debug", superHandler.GetDebug)
		r.Post("/superadmin/debug/toggle", superHandler.PostDebugToggle)
		r.Post("/superadmin/debug/clear", superHandler.PostDebugClear)
		r.Post("/superadmin/reindex", superHandler.PostReindexAll)
		r.Post("/superadmin/broadcast", superHandler.PostBroadcast)
		r.Post("/superadmin/community/create", superHandler.PostCreateCommunity)
		r.Post("/superadmin/community/delete", superHandler.PostDeleteCommunity)
		r.Post("/superadmin/community-request/approve", superHandler.PostApproveRequest)
		r.Post("/superadmin/community-request/deny", superHandler.PostDenyRequest)
		r.Post("/superadmin/platform-ai/grant", superHandler.PostGrantPlatformAI)
		r.Post("/superadmin/platform-ai/revoke", superHandler.PostRevokePlatformAI)
		r.Post("/superadmin/user/disable", superHandler.PostDisableUser)
		r.Post("/superadmin/user/enable", superHandler.PostEnableUser)
		r.Get("/superadmin/user/memberships", superHandler.GetUserMemberships)
		r.Post("/superadmin/user/sysban", superHandler.PostSystemBan)
		r.Post("/superadmin/user/community/ban", superHandler.PostCommunityBan)
		r.Post("/superadmin/user/community/remove", superHandler.PostCommunityRemove)
		r.Post("/superadmin/user/community/role", superHandler.PostCommunityRole)
	})

	r.Get("/explore", exploreHandler.GetIndex)
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Post("/explore/{slug}/request", exploreHandler.PostRequestJoin)
	})

	r.Get("/_debug/clock", func(w http.ResponseWriter, req *http.Request) {
		_ = webtempl.DebugClock().Render(req.Context(), w)
	})

	r.Get("/_debug/clock/stream", func(w http.ResponseWriter, req *http.Request) {
		clockStream(w, req, nc, log)
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Info("listening", "addr", cfg.HTTPAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("forumchat stopped")
	return nil
}

// htmlContentType sets text/html on navigation requests (Accept: text/html)
// before the handler writes, so the response compressor — which picks an
// encoder at WriteHeader time — sees a compressible type and engages on full
// pages. templ's Render otherwise leaves the type to Go's first-write content
// sniffing, which runs too late for the compressor and left pages (now
// carrying the inlined ~181KB app.css) shipping uncompressed.
//
// Scoped to text/html accepts so datastar SSE (text/event-stream) and asset
// requests are untouched; any handler that sets its own type still overrides
// this default before the first write.
func htmlContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		next.ServeHTTP(w, r)
	})
}

// immutableStatic stamps a long-lived immutable Cache-Control on static asset
// responses. Safe because every static URL carries a content hash (?v=<sha>):
// when a file's bytes change its URL changes, so a cached entry can never be
// wrong for the URL that produced it.
func immutableStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		next.ServeHTTP(w, r)
	})
}

func clockStream(w http.ResponseWriter, req *http.Request, nc *nats.Conn, log *slog.Logger) {
	sse := render.NewSSE(w, req)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	subject := "debug.clock"
	if nc != nil && nc.IsConnected() {
		ch := make(chan *nats.Msg, 8)
		sub, err := nc.ChanSubscribe(subject, ch)
		if err == nil {
			defer sub.Unsubscribe()
			go func() {
				for {
					select {
					case <-req.Context().Done():
						return
					case <-ticker.C:
						_ = nc.Publish(subject, []byte(time.Now().Format(time.RFC3339)))
					}
				}
			}()
			for {
				select {
				case <-req.Context().Done():
					return
				case m, ok := <-ch:
					if !ok {
						return
					}
					_ = render.PatchTempl(sse, webtempl.ClockFragment(string(m.Data)))
				}
			}
		}
		log.Warn("nats channel subscribe failed, falling back to local ticks", "err", err)
	}

	for {
		select {
		case <-req.Context().Done():
			return
		case <-ticker.C:
			_ = render.PatchTempl(sse, webtempl.ClockFragment(time.Now().Format(time.RFC3339)))
		}
	}
}

// buildIceServers turns the parsed config into the slice the rooms
// handler forwards to RTCPeerConnection. STUN-only is fine for same-LAN
// peers; symmetric-NAT guests need TURN or the connection silently
// stalls (no media despite signaling completing).
//
// All TURN URLs collapse into ONE credentialed ICEServer entry — the
// browser tries each transport (udp/tcp/tls) against the same allocation.
// The deprecated singular ROOMS_TURN_URL is merged in so existing deploys
// keep working after the move to ROOMS_TURN_URLS.
func buildIceServers(cfg config.Config) []rooms.ICEServer {
	var out []rooms.ICEServer
	if urls := nonEmpty(cfg.STUNURLs); len(urls) > 0 {
		out = append(out, rooms.ICEServer{URLs: urls})
	}
	turnURLs := nonEmpty(cfg.TURNURLs)
	if cfg.TURNURL != "" {
		turnURLs = append(turnURLs, cfg.TURNURL)
	}
	if len(turnURLs) > 0 {
		out = append(out, rooms.ICEServer{
			URLs:       turnURLs,
			Username:   cfg.TURNUsername,
			Credential: cfg.TURNPassword,
		})
	}
	return out
}

// nonEmpty returns a copy of in with blank entries dropped.
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// compressibleContentTypes lists MIME types the response compressor will
// encode. text/event-stream is included so long-lived datastar SSE streams
// ride the same brotli/zstd path as regular pages.
var compressibleContentTypes = []string{
	"text/css",
	"text/html",
	"text/plain",
	"text/javascript",
	"application/javascript",
	"application/x-javascript",
	"application/json",
	"application/atom+xml",
	"application/rss+xml",
	"image/svg+xml",
	"text/event-stream",
}

// newCompressor returns a single chi Compressor with brotli and zstd encoders
// registered on top of the built-in gzip/deflate. Most-recently-registered
// encoder wins precedence, so zstd is preferred when the client advertises it.
//
// chi's constructor level (5) governs zstd. Brotli is pinned to quality 4 with
// a 1 MiB window (LGWin=20) inside its EncoderFunc: chat fatMorph re-renders
// ~100 messages per send (~50–200 KB of HTML); brotli q5 + the default 4 MiB
// window pushed PostSend to ~500 ms. q4 + LGWin=20 trades ~3–5% ratio for
// roughly half the encode CPU and a smaller per-stream working set, which
// matters because chi pools encoders per concurrent connection.
//
// SSE handlers must call httpx.PrimeSSE(w) before datastar.NewSSE so this
// compressor's WriteHeader hook picks an encoder and sets Content-Encoding
// before the SDK's ResponseController.Flush unwraps past the wrapper.
func newCompressor() *middleware.Compressor {
	const brotliQuality = 4
	const brotliLGWin = 20

	c := middleware.NewCompressor(5, compressibleContentTypes...)
	// Register zstd first, br second — chi's SetEncoder prepends, so the
	// last-registered encoder wins precedence. br ends up preferred over zstd
	// over the built-in gzip/deflate when the client advertises all of them.
	c.SetEncoder("zstd", func(w io.Writer, level int) io.Writer {
		zw, err := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
		if err != nil {
			return nil
		}
		return zw
	})
	c.SetEncoder("br", func(w io.Writer, _ int) io.Writer {
		return brotli.NewWriterOptions(w, brotli.WriterOptions{
			Quality: brotliQuality,
			LGWin:   brotliLGWin,
		})
	})
	return c
}

// capRefContent trims expanded $-reference content so a single referenced
// thread can't blow the model's context window.
func capRefContent(s string) string {
	const max = 6000
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[:max] + "\n…(truncated)…"
	}
	return s
}

// formatChatForSummary renders a channel's most recent messages oldest-first as
// "Name: text" lines for the /summary agent prompt. Caps the length so the
// prompt stays within model context, keeping the most recent messages.
func formatChatForSummary(repo *chat.Repo, ctx context.Context, channelID string) string {
	msgs, err := repo.Recent(ctx, channelID, 300)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for i := len(msgs) - 1; i >= 0; i-- { // Recent is newest-first → emit oldest-first
		m := msgs[i]
		if m.DeletedAt != nil {
			continue
		}
		body := strings.TrimSpace(m.BodyMarkdown)
		if body == "" {
			continue
		}
		name := strings.TrimSpace(m.AuthorName)
		if name == "" {
			name = "User"
		}
		b.WriteString(name)
		b.WriteString(": ")
		b.WriteString(body)
		b.WriteByte('\n')
	}
	s := b.String()
	const maxChars = 16000
	if len(s) > maxChars {
		s = "…(earlier messages truncated)…\n" + s[len(s)-maxChars:]
	}
	return s
}

// recentChannelImages collects up to limit base64-encoded image payloads from a
// channel's most recent messages (newest first) for a vision /resume agent.
// Non-image attachments are skipped; unreadable uploads are silently dropped so
// one bad file never breaks the summary.
func recentChannelImages(ctx context.Context, repo *chat.Repo, store *uploads.Store, channelID string, limit int) []string {
	msgs, err := repo.Recent(ctx, channelID, 300) // newest-first
	if err != nil {
		return nil
	}
	out := make([]string, 0, limit)
	for _, m := range msgs {
		if m.DeletedAt != nil {
			continue
		}
		for _, att := range m.Attachments {
			if att.Kind != "image" {
				continue
			}
			u, err := store.Get(ctx, att.UploadID)
			if err != nil {
				continue
			}
			data, err := os.ReadFile(store.PathFor(u))
			if err != nil {
				continue
			}
			out = append(out, base64.StdEncoding.EncodeToString(data))
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}
