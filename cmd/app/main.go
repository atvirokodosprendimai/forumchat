package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/nats-io/nats.go"

	"github.com/atvirokodosprendimai/forumchat/internal/config"
	"github.com/atvirokodosprendimai/forumchat/internal/httpx"
	"github.com/atvirokodosprendimai/forumchat/internal/natsx"
	"github.com/atvirokodosprendimai/forumchat/internal/render"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
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

	nc, err := natsx.Connect(cfg.NATSURL, log)
	if err != nil {
		log.Warn("nats connect failed, continuing without nats", "err", err)
	}
	if nc != nil {
		defer nc.Drain()
	}

	r := chi.NewRouter()
	r.Use(httpx.Recover(log))
	r.Use(httpx.RequestLogger(log))

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("./web/static"))))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		_ = webtempl.Hello("world").Render(req.Context(), w)
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
