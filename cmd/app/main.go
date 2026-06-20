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
	"strings"
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
	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/bookmarks"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/dashboard"
	"github.com/atvirokodosprendimai/forumchat/internal/explore"
	"github.com/atvirokodosprendimai/forumchat/internal/forum"
	"github.com/atvirokodosprendimai/forumchat/internal/history"
	"github.com/atvirokodosprendimai/forumchat/internal/httpx"
	"github.com/atvirokodosprendimai/forumchat/internal/invites"
	"github.com/atvirokodosprendimai/forumchat/internal/lobbies"
	"github.com/atvirokodosprendimai/forumchat/internal/mailbox"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/presence"
	"github.com/atvirokodosprendimai/forumchat/internal/privatemsg"
	"github.com/atvirokodosprendimai/forumchat/internal/projects"
	"github.com/atvirokodosprendimai/forumchat/internal/push"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/rooms"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
	"github.com/atvirokodosprendimai/forumchat/internal/superadmin"
	"github.com/atvirokodosprendimai/forumchat/internal/timebudget"
	"github.com/atvirokodosprendimai/forumchat/internal/todos"
	"github.com/atvirokodosprendimai/forumchat/internal/uploads"
	"github.com/atvirokodosprendimai/forumchat/internal/worklog"
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
	r.Use(newCompressor().Handler)
	r.Use(sessions.LoadAndSave)
	r.Use(auth.Loader(sessions, aRepo, superAdmins))
	// Stash the request path so the sidebar can mark the active link
	// server-side (replaces the client DOM-walk that used to live in nav.js).
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(webtempl.WithCurrentPath(req.Context(), req.URL.Path)))
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
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("./web/static"))))

	// Serve the push service worker from the site root so it can claim
	// the whole '/' scope. Without this, registering /static/sw.js
	// confines its scope to /static/* and the push events never fire on
	// app routes. Also set Service-Worker-Allowed for belt-and-braces.
	r.Get("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Service-Worker-Allowed", "/")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, "./web/static/sw.js")
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

	uploadStore := uploads.NewStore(db, cfg.UploadsDir, cfg.UploadsMaxSize, cfg.UploadsSignKey)
	uploadHandler := &uploads.Handler{
		Store:       uploadStore,
		CommunityID: bootCommunity.ID,
		Log:         log,
		Sessions:    sessions, // lets project-share guests view images
	}

	chatRepo := chat.NewRepo(db)
	chatSvc := chat.NewService(chatRepo)
	chatBus := chat.NewBus()
	chatNewMsgBus := chat.NewBus()
	chatHandler := &chat.Handler{
		Svc:           chatSvc,
		Repo:          chatRepo,
		NATS:          nc,
		Bus:           chatBus,
		NewMsgBus:     chatNewMsgBus,
		Uploads:       uploadStore,
		AuthRepo:      aRepo,
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

	adminHandler := &admin.Handler{
		Repo:          aRepo,
		Svc:           svc,
		Chat:          chatHandler,
		Communities:   cRepo,
		Roster:        presenceTracker,
		Mail:          mailer,
		BaseURL:       cfg.BaseURL,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
		Log:           log,
	}

	dashboardHandler := &dashboard.Handler{Communities: cRepo, Log: log}
	exploreHandler := &explore.Handler{Communities: cRepo, AuthRepo: aRepo, Sessions: sessions, Log: log}

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

	// ----- Agent (per-community AI chat) -----------------------------------
	agentRepo := agent.NewRepo(db)
	agentBus := agent.NewBus()
	agentRunner := agent.NewRunner(agentRepo, agentBus, nc, log)
	agentHandler := &agent.Handler{
		Repo:          agentRepo,
		Svc:           agent.NewService(agentRepo),
		Runner:        agentRunner,
		Bus:           agentBus,
		NATS:          nc,
		Uploads:       uploadStore,
		Log:           log,
		CommunityID:   bootCommunity.ID,
		CommunityName: bootCommunity.Name,
	}
	if cfg.AIEnabled {
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
		agentHandler.ShareToChannel = func(ctx context.Context, communityID, channelSlug, authorID, bodyMD string) (string, error) {
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
	}
	webtempl.AIEnabled = cfg.AIEnabled

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

	// Wire the /resume chat slash command: summarise the channel's last 300
	// messages with an agent (in a public agent thread) and post the recap
	// back. Closure bridges chat → agent → chat without an import cycle. Only
	// when the Agent feature is on.
	if cfg.AIEnabled {
		chatHandler.Resume = func(ctx context.Context, communityID, channelID, requesterID, requesterName string) {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()

			// Posts the recap as a system ("yellow") message into the channel,
			// authored by no one — not the requester.
			post := func(body string) {
				if _, err := chatSvc.PostSystemMarkdown(ctx, communityID, channelID, body); err != nil {
					log.Warn("resume: post back", "err", err)
					return
				}
				chatBus.Broadcast(channelID)
				chatNewMsgBus.Broadcast(channelID)
				if nc != nil && nc.IsConnected() {
					_ = nc.Publish(natsx.ChatSubject(communityID), []byte(channelID))
					_ = nc.Publish(natsx.ChatNewSubject(communityID), []byte("new"))
				}
			}

			agents, _ := agentRepo.ListEnabledAgents(ctx, communityID)
			if len(agents) == 0 {
				post("🤖 No AI agent is enabled — an admin can add one in Admin → AI.")
				return
			}
			convo := formatChatForResume(chatRepo, ctx, channelID)
			if strings.TrimSpace(convo) == "" {
				post("🤖 Nothing to resume yet.")
				return
			}
			chName := "this channel"
			if ch, err := chatRepo.ChannelByID(ctx, channelID); err == nil {
				chName = "#" + ch.Name
			}
			prompt := "Summarise this chat conversation from " + chName + ". Give a concise recap as short bullet points: key topics, any decisions, and open questions.\n\n" + convo
			threadID, answer, err := agentHandler.Svc.SummarizeToThread(ctx, communityID, requesterID, agents[0], "Resume of "+chName, prompt)
			if err != nil || strings.TrimSpace(answer) == "" {
				log.Warn("resume: generate", "err", err)
				post("🤖 Couldn't generate a resume right now.")
				return
			}
			// Build the header (with the thread link) FIRST, then the answer.
			// The link must precede the answer: an LLM reply can end with a
			// dangling/unclosed code fence that would otherwise swallow a
			// trailing link and render it as a literal URL.
			header := "🤖 **Resume of the last 300 messages** _(requested by " + requesterName + ")_"
			if c, err := cRepo.ByID(ctx, communityID); err == nil && threadID != "" {
				base := strings.TrimRight(cfg.BaseURL, "/")
				header += " · [↗ View in Agent thread](" + base + "/c/" + c.Slug + "/agent/" + threadID + ")"
			}
			post(header + "\n\n" + answer)
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

		// Bare /chat redirects to #general so the URL always carries a
		// channel slug. Channel-agnostic actions (upload, mention search,
		// cross-page events, extract) stay at /chat/*; static segments win
		// over the {channel} wildcard in chi so they're never shadowed.
		r.Get("/chat", chatHandler.GetChatRedirect)
		r.Post("/chat/upload", chatHandler.PostUpload)
		r.Get("/chat/mention", chatHandler.GetMentionSearch)
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
		r.Post("/chat/{channel}/send", chatHandler.PostSend)
		r.Post("/chat/{channel}/read", chatHandler.PostMarkRead)
		r.Post("/block", chatHandler.PostBlock)
		r.Post("/unblock", chatHandler.PostUnblock)
		r.Post("/report", chatHandler.PostReport)

		// Agent — per-community AI chat with threads + history. Static
		// segments (new) win over the {thread} wildcard in chi.
		if cfg.AIEnabled {
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
		}

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
			r.Post("/forum/{id}/hard-delete", forumHandler.PostHardDeleteThread)
			if cfg.MailboxEnabled && mailboxHandler != nil {
				r.Get("/admin/mail-filters", mailboxHandler.GetCommunityFilters)
				r.Post("/admin/mail-filters", mailboxHandler.PostCommunityFilterCreate)
				r.Post("/admin/mail-filters/{id}/delete", mailboxHandler.PostCommunityFilterDelete)
				r.Post("/admin/mail-filters/{id}/apply", mailboxHandler.PostCommunityFilterApply)
			}
			if cfg.AIEnabled {
				r.Get("/admin/ai", agentHandler.GetAgents)
				r.Get("/admin/ai/new", agentHandler.GetNewAgentForm)
				r.Get("/admin/ai/{id}/edit", agentHandler.GetEditAgentForm)
				r.Post("/admin/ai", agentHandler.PostSaveAgent)
				r.Post("/admin/ai/{id}/delete", agentHandler.PostDeleteAgent)
			}
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
			r.Use(community.LoadCommunity(cRepo))

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
		r.Use(community.LoadCommunity(cRepo))

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
	if cfg.MailboxEnabled && mailboxHandler != nil {
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireAuth)
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

	// Platform super-admin surface — global god-mode over every community
	// and user, gated by the SUPERADMIN_EMAILS allowlist.
	superHandler := &superadmin.Handler{AuthRepo: aRepo, Communities: cRepo, Log: log, Bus: chatHandler.Bus}
	r.Group(func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Use(auth.RequireSuperAdmin)
		r.Get("/superadmin", superHandler.GetIndex)
		r.Post("/superadmin/community/create", superHandler.PostCreateCommunity)
		r.Post("/superadmin/community/delete", superHandler.PostDeleteCommunity)
		r.Post("/superadmin/user/disable", superHandler.PostDisableUser)
		r.Post("/superadmin/user/enable", superHandler.PostEnableUser)
		r.Get("/superadmin/user/memberships", superHandler.GetUserMemberships)
		r.Post("/superadmin/user/sysban", superHandler.PostSystemBan)
		r.Post("/superadmin/user/community/ban", superHandler.PostCommunityBan)
		r.Post("/superadmin/user/community/remove", superHandler.PostCommunityRemove)
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
func buildIceServers(cfg config.Config) []rooms.ICEServer {
	var out []rooms.ICEServer
	if len(cfg.STUNURLs) > 0 {
		urls := make([]string, 0, len(cfg.STUNURLs))
		for _, u := range cfg.STUNURLs {
			if u != "" {
				urls = append(urls, u)
			}
		}
		if len(urls) > 0 {
			out = append(out, rooms.ICEServer{URLs: urls})
		}
	}
	if cfg.TURNURL != "" {
		out = append(out, rooms.ICEServer{
			URLs:       []string{cfg.TURNURL},
			Username:   cfg.TURNUsername,
			Credential: cfg.TURNPassword,
		})
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

// formatChatForResume renders a channel's most recent messages oldest-first as
// "Name: text" lines for the /resume agent prompt. Caps the length so the
// prompt stays within model context, keeping the most recent messages.
func formatChatForResume(repo *chat.Repo, ctx context.Context, channelID string) string {
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
