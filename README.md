# forumchat

A web-based community space that blends the **moment** of Discord/Telegram chat
with the **memory** of a classic forum.

One realtime chat channel keeps people present; forum threads keep durable
conversation. Every new thread auto-announces itself as a system message in the
chat channel so the community never misses topics worth discussing.

> Single-community-now, multi-community-ready in the data model. **No DMs** —
> communication is public to the community by design.

---

## Features

- **Auth**: invite-code gated registration + email verification + cookie sessions.
- **One realtime chat channel** per community. Persistent (SQLite), live (NATS pub/sub + datastar SSE), lazy scrollback.
- **Forum** with threads, flat replies, optional single-parent quote, 15-min grace window for self-delete.
- **Thread → chat bridge**: creating a thread posts a system message in chat with a clickable link. One-way; replies don't echo back.
- **Online presence list** per community, TTL-swept, live-updated via SSE.
- **Moderation**: admin / moderator / member roles. Soft-delete with role-differentiated render (mods see deleted content, others see placeholder). Ban kicks active sessions.
- **Image uploads**: sha256 content-addressed, HMAC-signed time-limited URLs bound to viewer + community.
- **Server-rendered Markdown** via goldmark + bluemonday sanitiser, shared by chat and forum.
- **Single Go binary**, multi-stage Docker image, docker compose stack (`app + nats + mailpit`).

### Out of MVP (intentionally)

- No DMs, no nested reply trees, no multiple chat channels per community, no federation.
- No OAuth (Google/Facebook) yet — schema reserves room for linked identities.
- No JetStream replay; reconnects catch up via the DB.
- No drag-drop upload UI; clients paste the returned markdown from `POST /uploads`.

---

## Tech stack

| Layer            | Choice                                                      |
|------------------|-------------------------------------------------------------|
| Language         | Go 1.25 (toolchain 1.26)                                    |
| HTTP router      | `github.com/go-chi/chi/v5`                                  |
| Templating       | `github.com/a-h/templ`                                      |
| Realtime UI      | [Datastar](https://data-star.dev) over Server-Sent Events   |
| Messaging        | NATS core pub/sub (`github.com/nats-io/nats.go`)            |
| DB               | SQLite via `modernc.org/sqlite` (CGO-free), WAL             |
| Migrations       | `github.com/pressly/goose/v3` (embedded)                    |
| Markdown         | `yuin/goldmark` + `microcosm-cc/bluemonday`                 |
| Sessions         | `github.com/alexedwards/scs/v2` (memstore in MVP)           |
| Password hash    | bcrypt cost 12                                              |
| Rate limit       | `github.com/go-chi/httprate`                                |
| Email (dev)      | SMTP → mailpit (UI at `localhost:8025`)                     |
| Containers       | Distroless `nonroot`, multi-stage Dockerfile                |

---

## Quick start

### Docker compose (recommended)

```sh
cp .env.example .env       # optional; defaults work for compose
make up                    # builds app, starts app + nats + mailpit
./bin/cli-in-compose ...   # see "CLI" below — invite to bootstrap
```

Open:

- App:     <http://localhost:8080>
- Mailpit: <http://localhost:8025>  (catches verification emails in dev)
- NATS:    `nats://localhost:4222`

### Local dev (no Docker)

```sh
make tidy          # go mod tidy
make gen           # templ generate
make run           # go run ./cmd/app
```

Without NATS, the app boots and serves; chat works for the local sender (no fan-out
to other tabs/processes). Start a NATS sidecar to get realtime fan-out:

```sh
docker run -d --name nats -p 4222:4222 nats:2.10-alpine -js
```

Mailpit for local dev:

```sh
docker run -d --name mailpit -p 1025:1025 -p 8025:8025 axllent/mailpit
```

If neither SMTP nor mailpit is reachable, the app falls back to a `LogMailer` —
verification URLs appear in the application log so you can copy/paste them.

---

## First-time bootstrap

Sign-up is invite-only. After the app is running:

```sh
# 1. Issue an invite code (prints the code on stdout).
./bin/forumchat-cli invite 1

# 2. Visit http://localhost:8080/register, supply email + password + the code.
# 3. Open the verification link (mailpit at :8025, or grep the app log for verify_url).
# 4. You're verified, signed in, and a member.

# 5. Promote yourself to admin so you can ban / delete via the UI.
./bin/forumchat-cli role you@example.com admin
```

---

## Configuration

All configuration is via environment variables (or a `.env` file in the working
directory, auto-loaded by godotenv).

| Variable             | Default                                | Purpose                                                            |
|----------------------|----------------------------------------|--------------------------------------------------------------------|
| `ENV`                | `dev`                                  | `dev` or `prod`. Toggles slog text/json + cookie `Secure` flag.    |
| `HTTP_ADDR`          | `:8080`                                | Listen address.                                                    |
| `BASE_URL`           | `http://localhost:8080`                | Public origin. Used to build verification URLs and bridge links.    |
| `DB_PATH`            | `./data/forumchat.db`                  | SQLite file (auto-creates parent dir).                             |
| `MIGRATE_ON_BOOT`    | `true`                                 | Auto-run goose migrations at startup. Set `false` in prod CI/CD.   |
| `NATS_URL`           | `nats://127.0.0.1:4222`                | NATS connection URL; app degrades gracefully if unreachable.        |
| `SESSION_KEY`        | dev placeholder                        | scs cookie signing key. **Must not contain `dev-only` in prod.**   |
| `SESSION_MAX_AGE`    | `720h` (30 days)                       | Cookie lifetime + idle timeout.                                    |
| `UPLOADS_DIR`        | `./uploads`                            | Local-disk uploads root (content-addressed sha256).                |
| `UPLOADS_MAX_BYTES`  | `5242880` (5 MiB)                      | Per-upload size cap.                                               |
| `UPLOADS_SIGN_KEY`   | dev placeholder                        | HMAC key for signed URLs. **Must not contain `dev-only` in prod.** |
| `SMTP_HOST`          | `127.0.0.1`                            | SMTP relay for verification email. Set to `skip` (or any unreachable host) to force LogMailer fallback. |
| `SMTP_PORT`          | `1025`                                 | SMTP relay port (mailpit defaults).                                |
| `SMTP_USER`          | empty                                  | Optional PLAIN auth user.                                          |
| `SMTP_PASS`          | empty                                  | Optional PLAIN auth password.                                      |
| `SMTP_FROM`          | `forumchat@localhost`                  | From header for verification mail.                                 |
| `COMMUNITY_SLUG`     | `main`                                 | Slug of the bootstrap community (idempotent first-run).            |
| `COMMUNITY_NAME`     | `The Community`                        | Human-friendly name.                                               |
| `PRESENCE_TTL`       | `30s`                                  | Seconds since last heartbeat after which a user drops from the online list. |
| `EDIT_GRACE`         | `15m`                                  | Window in which authors can self-delete their own thread / post / chat message. |

In production (`ENV=prod`), boot fails fast if `SESSION_KEY` or `UPLOADS_SIGN_KEY`
still contain `dev-only`.

---

## CLI

```sh
forumchat-cli invite [count]
forumchat-cli role  <email> <member|moderator|admin>
forumchat-cli ban   <email> [duration]      # e.g. 24h; omit for permanent
forumchat-cli unban <email>
```

The CLI shares the same `.env`/env-vars as the app — point `DB_PATH` /
`COMMUNITY_SLUG` at the same values you boot the app with. Inside compose, run via
`docker compose exec app /app/forumchat-cli ...` (after copying the binary into
the image — current image ships the app binary only; running CLI from the host
against the volume-mounted DB also works for dev).

---

## HTTP routes

| Method | Path                              | Auth     | Notes                                       |
|-------:|-----------------------------------|----------|---------------------------------------------|
| GET    | `/healthz`                        | none     | Liveness probe.                             |
| GET    | `/`                               | optional | Home (signed-in greeting or signup CTA).    |
| GET    | `/register`                       | none     | Registration form.                          |
| POST   | `/register`                       | none     | Rate-limited 10/min/IP.                     |
| GET    | `/verify?token=…`                 | none     | Activates account + auto sign-in.           |
| GET    | `/login`                          | none     |                                             |
| POST   | `/login`                          | none     | Rate-limited 10/min/IP.                     |
| POST   | `/logout`                         | session  |                                             |
| GET    | `/profile`                        | required | Edit display name + avatar URL.             |
| POST   | `/profile`                        | required |                                             |
| GET    | `/chat`                           | required | Chat page (last 50 msgs).                   |
| POST   | `/chat/send`                      | required | Persist + NATS fan-out + SSE patch back.    |
| GET    | `/chat/stream`                    | required | datastar SSE stream of new chat fragments.  |
| GET    | `/chat/older?before=<RFC3339Nano>`| required | Lazy scrollback via SSE prepend.            |
| POST   | `/chat/delete?id=…`               | mod+     | Soft-delete chat message.                   |
| GET    | `/forum`                          | required | Thread index (by `last_activity_at desc`).  |
| GET    | `/forum/new`                      | required | New-thread form.                            |
| POST   | `/forum/new`                      | required | Creates thread + bridge system message.     |
| GET    | `/forum/{id}`                     | required | Thread view + replies.                      |
| POST   | `/forum/{id}/reply`               | required | Optional `quoted_post_id`.                  |
| POST   | `/forum/{id}/delete`              | required | Author within grace, or mod+.               |
| POST   | `/forum/post/{id}/delete`         | required | Author within grace, or mod+.               |
| GET    | `/presence/stream`                | required | datastar SSE pushing the online-list aside. |
| POST   | `/uploads`                        | required | Multipart `file=` → markdown `![](signed-url)`. |
| GET    | `/uploads/{id}?exp=&sig=`         | required | HMAC-verified, viewer-bound, community-scoped. |
| GET    | `/static/*`                       | none     | Static assets.                              |
| GET    | `/_debug/clock`, `/_debug/clock/stream` | none | Datastar+NATS smoke page.                   |

---

## Project layout

```
cmd/
  app/main.go                  # HTTP server entrypoint, wiring
  cli/main.go                  # forumchat-cli (invite / role / ban / unban)
internal/
  config/                      # env-driven config + slog setup
  storage/sqlite/              # DB open + embedded goose migrations
    migrations/00001_init.sql  # schema (9 tables)
  natsx/                       # NATS connect + subject helpers
  render/                      # markdown pipeline + datastar SSE helper
  httpx/                       # request logger + recover middleware
  auth/                        # users, sessions, register/verify/login, ban
  community/                   # bootstrap + membership lookup
  chat/                        # chat domain + handlers (NATS + SSE)
  forum/                       # threads + posts + handlers (+ bridge to chat)
  presence/                    # in-process tracker + SSE handler
  uploads/                     # sha256 store + HMAC signed-URL handler
web/
  templ/                       # *.templ → generated *_templ.go
  static/app.css               # base styles
migrations/                    # reserved (real migrations live under internal/storage/sqlite/migrations)
Dockerfile
docker-compose.yml
Makefile
.env.example
```

### Schema (SQLite)

`users`, `verification_tokens`, `communities`, `memberships`, `invite_codes`,
`chat_messages`, `threads`, `posts`, `uploads`. Every multi-tenant row carries
`community_id` so adding a community picker later is a UI change, not a
migration. `chat_messages.kind` + `author_id NULL` covers system messages such
as `thread_announce`. `memberships.trust_level` is reserved for post-MVP
Discourse-style trust tiers.

---

## Make targets

```sh
make tidy   # go mod tidy
make gen    # templ generate (runs *.templ → *_templ.go)
make build  # build single binary into bin/forumchat
make run    # gen then go run ./cmd/app
make dev    # alias for run
make up     # docker compose up -d --build
make down   # docker compose down
make logs   # docker compose logs -f app
make fmt    # go fmt ./...
make vet    # go vet ./...
make test   # go test ./...
```

---

## Tests

```sh
go test ./...
```

Covered today (boundary tests, not exhaustive):

- `internal/auth`: password hash round-trip + short-password rejection;
  end-to-end register → verify → login service flow; bad/used invite codes;
  login when unverified / wrong password.
- `internal/uploads`: save + sign + verify round-trip; bad-MIME rejection;
  oversize rejection.

Realtime chat fan-out is verified manually (two browsers on `/chat`). A NATS
test fixture for the two-client smoke is on the future list.

---

## Production notes

- Build the image: `docker build -t forumchat:latest .`
- Run behind a TLS terminator; set `ENV=prod`, `BASE_URL=https://your.host`.
- Set `SESSION_KEY` and `UPLOADS_SIGN_KEY` to long random strings — boot will
  reject defaults that still contain `dev-only`.
- `MIGRATE_ON_BOOT=false` if you prefer to run migrations from CI/CD.
- Sessions are in-process (scs memstore) — a restart logs everyone out. For
  multi-instance deployment, swap in a persistent `scs.Store`.
- Uploads live on local disk under `UPLOADS_DIR`. For multi-instance, mount a
  shared volume or replace with an S3-compatible store (the `uploads.Store`
  interface makes this a focused refactor).
- Email defaults to a single relay; for production set `SMTP_HOST/PORT/USER/PASS/FROM`.

---

## Roadmap

Tracked in `eidos/spec - forumchat - community web app with realtime chat and forum threads.md`
under `## Future`:

- OAuth (Google → Facebook), linked to existing global identities.
- Multi-community UI (picker + per-community invites).
- JetStream-backed chat with replay on reconnect.
- Trust levels and per-trust-level capabilities.
- Push / web notifications for thread replies.
- Full-text search across chat + forum (SQLite FTS5).
- S3-compatible upload backend.
- Drag-drop upload UI.
- Edit history surfaced to users.

---

## License

No license declared yet — add one before public release.
