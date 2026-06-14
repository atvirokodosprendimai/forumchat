package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/httprate"
	"github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/admin"
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/bookmarks"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/dashboard"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
	"github.com/atvirokodosprendimai/forumchat/internal/history"
	"github.com/atvirokodosprendimai/forumchat/internal/invites"
	"github.com/atvirokodosprendimai/forumchat/internal/httpx"
	"github.com/atvirokodosprendimai/forumchat/internal/presence"
	"github.com/atvirokodosprendimai/forumchat/internal/privatemsg"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/todos"
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

	cRepo := community.NewRepo(db)
	bootCommunity, err := cRepo.BootstrapOrFetch(ctx, cfg.CommunitySlug, cfg.CommunityName)
	if err != nil {
		return fmt.Errorf("bootstrap community: %w", err)
	}
	log.Info("community ready", "slug", bootCommunity.Slug, "id", bootCommunity.ID)

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
			From: cfg.SMTPFrom,
			TLSMode: cfg.SMTPTLS, TLSSkip: cfg.SMTPTLSSkip,
			Log: log,
		}
	} else {
		mailer = &auth.LogMailer{Log: log}
	}
	svc := &auth.Service{
		Repo:      aRepo,
		Mailer:    mailer,
		BaseURL:   cfg.BaseURL,
		VerifyTTL: 48 * time.Hour,
		InviteTTL: 30 * 24 * time.Hour,
	}
	sessions := auth.NewSessionManager(cfg.SessionMaxAge, cfg.IsProd())
	// Persistent sessions in SQLite so users stay signed in across restarts.
	sessions.Store = auth.NewSQLStore(ctx, db)
	authHandler := &auth.Handler{
		Svc:           svc,
		Repo:          aRepo,
		Sessions:      sessions,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}

	r := chi.NewRouter()
	r.Use(httpx.Recover(log))
	r.Use(httpx.RequestLogger(log))
	r.Use(sessions.LoadAndSave)
	r.Use(auth.Loader(sessions, aRepo))

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
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("./web/static"))))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Rate-limit auth endpoints (10 req/min/IP) and chat send (30 req/min/user).
	r.Group(func(r chi.Router) {
		r.Use(httprate.LimitByIP(10, time.Minute))
		r.Post("/login", authHandler.PostLogin)
		r.Post("/register", authHandler.PostRegister)
		r.Post("/register-as-admin", authHandler.PostRegisterAsAdmin)
	})
	r.Get("/register", authHandler.GetRegister)
	r.Get("/register-as-admin", authHandler.GetRegisterAsAdmin)
	r.Get("/login", authHandler.GetLogin)
	r.Get("/verify", authHandler.GetVerify)
	r.Post("/logout", authHandler.PostLogout)

	uploadStore := uploads.NewStore(db, cfg.UploadsDir, cfg.UploadsMaxSize, cfg.UploadsSignKey)
	uploadHandler := &uploads.Handler{Store: uploadStore, CommunityID: bootCommunity.ID, Log: log}

	chatRepo := chat.NewRepo(db)
	chatSvc := chat.NewService(chatRepo)
	chatBus := chat.NewBus()
	chatHandler := &chat.Handler{
		Svc:           chatSvc,
		Repo:          chatRepo,
		NATS:          nc,
		Bus:           chatBus,
		Uploads:       uploadStore,
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
		Tracker: presenceTracker, CommunityID: bootCommunity.ID, Log: log,
	}

	forumRepo := forum.NewRepo(db)
	forumSvc := forum.NewService(forumRepo, cfg.EditGrace)
	forumBus := forum.NewBus()
	forumHandler := &forum.Handler{
		Svc:           forumSvc,
		Repo:          forumRepo,
		Chat:          chatSvc,
		ChatRepo:      chatRepo,
		ChatBus:       chatBus,
		Bus:           forumBus,
		NATS:          nc,
		Uploads:       uploadStore,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		BaseURL:       cfg.BaseURL,
		Log:           log,
	}

	adminHandler := &admin.Handler{
		Repo:          aRepo,
		Svc:           svc,
		Chat:          chatHandler,
		Communities:   cRepo,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}

	dashboardHandler := &dashboard.Handler{Communities: cRepo, Log: log}

	todosHandler := &todos.Handler{Repo: todos.NewRepo(db), ChatRepo: chatRepo, Forum: forumRepo, Log: log}

	invitesHandler := &invites.Handler{AuthRepo: aRepo, Chat: chatHandler, Sessions: sessions, Log: log}

	// Authenticated but not-yet-approved members: only /, /pending, /logout, /profile.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Get("/pending", authHandler.GetPending)
		r.Get("/profile", authHandler.GetProfile)
		r.Post("/profile", authHandler.PostProfile)
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

	pmRepo := privatemsg.NewRepo(db)
	pmBus := privatemsg.NewBus()
	pmSvc := &privatemsg.Service{Repo: pmRepo, Bus: pmBus}
	pmHandler := &privatemsg.Handler{
		Svc:      pmSvc,
		Repo:     pmRepo,
		Bus:      pmBus,
		AuthRepo: aRepo,
		Sessions: sessions,
		Log:      log,
	}

	// Per-community JOIN landing — LoadCommunity runs so the templ can render
	// the community name, but RequireMember does NOT (this is the path that
	// admits new members). Mounted before the main /c/{slug} group so it
	// doesn't get caught by RequireMember.
	r.Route("/c/{slug}/join", func(r chi.Router) {
		r.Use(community.LoadCommunity(cRepo))
		r.Get("/", invitesHandler.GetJoin)
		r.Post("/confirm", invitesHandler.PostJoinConfirm)
		r.Post("/set-password", invitesHandler.PostJoinSetPassword)
	})

	// Per-community area: every page, every SSE stream, every POST nests under
	// /c/{slug}. LoadCommunity resolves the slug; RequireMember rebinds the
	// viewer's identity to that community's membership row.
	r.Route("/c/{slug}", func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Use(community.LoadCommunity(cRepo))
		r.Use(community.RequireMember(aRepo))
		r.Use(auth.RequireApproved)

		r.Get("/chat", chatHandler.GetPage)
		r.Post("/chat/send", chatHandler.PostSend)
		r.Get("/chat/stream", chatHandler.GetStream)

		r.Get("/presence/stream", presenceHandler.GetStream)

		r.Post("/uploads", uploadHandler.PostUpload)

		r.Get("/forum", forumHandler.GetIndex)
		r.Get("/forum/new", forumHandler.GetNew)
		r.Post("/forum/new", forumHandler.PostNew)
		r.Get("/forum/{id}", forumHandler.GetThread)
		r.Get("/forum/{id}/stream", forumHandler.GetThreadStream)
		r.Post("/forum/{id}/reply", forumHandler.PostReply)
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

		r.Get("/todos", todosHandler.GetIndex)
		r.Post("/todos", todosHandler.PostCreate)
		r.Post("/todos/{id}/status", todosHandler.PostStatus)
		r.Post("/todos/{id}/delete", todosHandler.PostDelete)

		r.Group(func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleMod))
			r.Post("/chat/delete", chatHandler.PostDelete)
		})

		// Per-community admin area.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireRole(auth.RoleAdmin))
			r.Get("/admin", adminHandler.GetIndex)
			r.Post("/admin/approve", adminHandler.PostApprove)
			r.Post("/admin/reject", adminHandler.PostReject)
			r.Post("/admin/ban", adminHandler.PostBan)
			r.Post("/admin/unban", adminHandler.PostUnban)
			r.Post("/admin/invite", adminHandler.PostInvite)
			r.Post("/admin/invite/revoke", adminHandler.PostInviteRevoke)
			r.Post("/admin/add-member", adminHandler.PostAddMember)
			r.Post("/forum/{id}/hard-delete", forumHandler.PostHardDeleteThread)
		})
	})

	// Global admin (uses session's CurrentCommunity membership for the role
	// check) — community creation lives here so it isn't gated by being a
	// member of the new community itself.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Use(auth.RequireRole(auth.RoleAdmin))
		r.Post("/admin/create-community", adminHandler.PostCreateCommunity)
	})

	// Uploads GET lives at root so stored /uploads/{id}?sig=... URLs survive
	// the multi-community route restructure. The HMAC signature already
	// scopes access (binds upload id + viewer id + exp), so no
	// community-scoped middleware is needed.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Get("/uploads/{id}", uploadHandler.GetFile)
	})

	// Private messages are global — no community membership required.
	// The handler authenticates via the session directly.
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		pmHandler.Routes(r)
	})

	r.Get("/", dashboardHandler.GetIndex)

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
