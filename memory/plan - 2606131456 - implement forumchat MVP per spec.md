---
tldr: Build forumchat MVP per the spec — domain-by-domain after a small infra scaffold, light testing at boundaries, dockerfile + compose from start, single Go binary delivery.
status: active
---

# Plan: Implement forumchat MVP per spec

## Context

- Spec: [[spec - forumchat - community web app with realtime chat and forum threads]]
- Stack locked: Go 1.25 + chi + templ + datastar (SSE) + NATS core pub/sub + SQLite + goldmark/bluemonday + local-disk uploads.
- Repo state at plan time: only `go.mod` and empty `cmd/app/main.go`. Not a git repo yet.
- Side-flag: `go.mod` module path is `gihtub.com/atvirokodosprendimai/forumchat` — likely typo `gihtub` → `github`. Resolve in Phase 1 Action 1.
- Decisions taken in `/eidos:spec` round (recorded in MemPalace drawer `forumchat/spec-mvp-decisions`).
- Plan-shape decisions taken in `/eidos:plan` round:
  - Phasing: **domain-by-domain** (each phase = one domain done well).
  - Testing: **unit + a few integration tests at boundaries** (handlers, sanitiser, auth flow). Manual browser testing for realtime.
  - Deployment shape: **single binary + Dockerfile + docker compose from the start** (compose mirrors local dev: app + NATS + mailpit).

## Phases

### Phase 1 - Project scaffold & dev environment - status: completed

1. [x] Initialise git repo and fix `go.mod` module path
   - => `git init`, branch `task/phase-1-scaffold` (pre-tool branch hook blocks main).
   - => `.gitignore` covers binaries, uploads, sqlite, env, editor cruft.
   - => Module path already `github.com/atvirokodosprendimai/forumchat` — earlier "gihtub" reading was misread; no typo to fix.
2. [x] Create directory skeleton per spec
   - => Added `internal/{config,httpx}` beyond spec — config loader and HTTP middleware.
   - => `.gitkeep` placeholders in each empty dir for git tracking.
3. [x] Add core deps and tools
   - => chi v5.3.0, templ v0.3.1020, datastar-go v1.2.2 (moved from monorepo to dedicated repo `github.com/starfederation/datastar-go/datastar`), nats.go v1.52.0, modernc sqlite v1.52.0, goldmark v1.8.2, bluemonday v1.0.27, alexedwards/scs v2.9.0, bcrypt, goose v3.27.1, caarlos0/env/v11, godotenv, google/uuid.
   - => Used `go tool` directive (Go 1.24+) for templ and goose CLIs in `go.mod`. Tool deps brought in many indirect DB drivers (clickhouse, mssql, mysql, libsql, vertica, ydb) via goose — bloat in `go.sum`, no runtime cost, accept for MVP.
4. [x] Decide session library
   - => alexedwards/scs/v2 chosen over gorilla/sessions: simpler middleware fit, idiomatic for `net/http`, easy revocation via store deletion.
   - => Open: scs's bundled `sqlite3store` uses CGO `mattn/go-sqlite3`; conflicts with our `modernc.org/sqlite`. Phase 2 will use scs memstore for MVP — sessions don't survive restart, acceptable for invite-only community. Custom modernc-backed scs.Store deferred (Phase 8 polish or earlier if friction).
5. [x] Wire `cmd/app/main.go` minimum
   - => chi router with Recover + RequestLogger middleware (`internal/httpx`).
   - => Endpoints: `GET /healthz`, `GET /`, `GET /_debug/clock` + `GET /_debug/clock/stream` (SSE).
   - => slog text handler in dev, JSON in prod.
   - => Graceful shutdown on SIGINT/SIGTERM with 5s timeout.
6. [x] Dockerfile + docker compose
   - => Multi-stage `golang:1.26-alpine` build → `distroless/static-debian12:nonroot` runtime. CGO_ENABLED=0.
   - => Compose services: `app`, `nats:2.10-alpine` (JetStream enabled with `-js` for future use), `mailpit` for dev SMTP capture (port 8025 UI).
   - => Volumes `app-data` and `app-uploads` persistent.
   - => Makefile: tidy, gen, build, run, dev, up, down, logs, fmt, vet, test.
7. [x] SQLite + migrations bootstrap
   - => goose chosen (already added in Action 3). Embedded migrations under `internal/storage/sqlite/migrations/`.
   - => `00001_init.sql` creates all 9 tables from spec sketch: users, verification_tokens, communities, memberships, invite_codes, chat_messages (with `kind` + `author_id NULL` + `ref_thread_id` for system messages), threads, posts (with `quoted_post_id`), uploads. `community_id` on every multi-tenant row. `trust_level INTEGER NOT NULL DEFAULT 0` reserved.
   - => DB opens with WAL, busy_timeout=5000, foreign_keys=ON, synchronous=NORMAL. MaxOpenConns=1 (modernc + WAL: single writer pattern).
   - => Migrations run on boot when `MIGRATE_ON_BOOT=true` (default in dev, off in prod).
8. [x] NATS wiring
   - => `internal/natsx` connects with infinite reconnect, logs disconnect/reconnect/close.
   - => Subject helpers `ChatSubject`, `ForumSubject`, `PresenceSubject` (community-scoped).
   - => Boot continues gracefully when NATS unreachable — debug clock falls back to local ticks. Smoke handler `/_debug/publish` deferred (Phase 4 will cover with real chat usage).
9. [x] Datastar SSE base helper
   - => `render/sse.go` thin wrapper around datastar-go SDK: `NewSSE(w, r)` and `PatchTempl(sse, component, opts...)`.
   - => Smoke page `/_debug/clock` opens SSE via `data-on-load`, server publishes time to `debug.clock` NATS subject every second, subscriber patches `#clock` fragment. Falls back to local ticks if NATS down.
   - => Browser verification deferred until interactive session (smoke test confirmed HTTP-level: `/_debug/clock` returns 200 with templ-rendered page).

**Phase 1 exit:** ✓ `go build ./...` clean, `go vet ./...` clean, app boots, migrations apply, `/healthz` and `/` return 200, NATS-down graceful fallback works.

### Phase 2 - Auth & invite registration - status: completed

1. [x] Auth domain shape
   - => `internal/auth/{user,errors,password,tokens,repo,service,session,middleware,handlers,mailer}.go`.
   - => bcrypt cost 12. UUIDv4 IDs via google/uuid. Times stored as int64 unix-seconds.
2. [x] Invite code lifecycle
   - => `cmd/cli` subcommand `forumchat-cli invite [count]` generates codes for the bootstrap community.
   - => Codes 16-char base32 uppercase. `expires_at` default 30 days. `used_by` + `used_at` set on consume.
   - => Single-use enforced in `Repo.ConsumeInvite` (sets used_by + used_at atomically inside register tx).
3. [x] Registration flow
   - => GET/POST `/register` with templ form (email, password, invite_code).
   - => Service.Register orders: insert user (status=pending) → consume invite → create verification token (24-byte base32, 48h TTL). FK-safe ordering: user inserted before invite consume to satisfy `used_by` FK.
   - => Mailer interface with `SMTPMailer` (mailpit/smtp) and `LogMailer` (fallback when SMTP_HOST is empty). Verify URL logged in dev for easy CLI smoke.
4. [x] Email verification
   - => GET `/verify?token=…` consumes token, activates user, derives display name from email local-part, creates membership with role=member, signs the user in via session cookie.
5. [x] Login + sessions
   - => alexedwards/scs/v2 with memstore (in-process). Cookie name `forumchat_session`, HttpOnly, SameSite=Lax, Secure in prod.
   - => GET/POST `/login`, POST `/logout`. Bad password / unverified / banned all surface specific errors on form.
6. [x] Auth middleware + context
   - => `Loader` middleware reads session, loads user + membership into request context. Auto-logs-out on missing user/membership or ban (with `/login?banned=1` redirect).
   - => `RequireAuth` redirects to login with `?next=`. `RequireRole(min)` returns 403.
   - => `FromContext(ctx)` returns `Identity{User, Membership}`.
7. [x] Tests (boundary)
   - => `password_test.go`: bcrypt round-trip + short-password rejection.
   - => `service_test.go`: register→verify→login happy path, invalid invite, reused invite, login-when-unverified, login with bad password. SQLite tmpdir per test.
   - => HTTP smoke (manual via curl): /register, /verify, /, /logout, /login, bad-login form error, CLI ban → ban login error. All green.

**Phase 2 exit:** ✓ Tests pass. End-to-end auth smoke green. CLI manages invites + bans.
- => Known carry-overs:
  - => scs memstore loses sessions on restart — acceptable for invite-only MVP; custom modernc-backed scs.Store deferred.
  - => Email verification falls back to LogMailer when SMTP_HOST is empty — compose default uses mailpit.

### Phase 3 - Community bootstrap & membership - status: open

1. [ ] Bootstrap single community on first run
   - Idempotent: if `communities` empty, create one from env (`COMMUNITY_SLUG`, `COMMUNITY_NAME`).
2. [ ] Membership service
   - Auto-join verified user to the bootstrap community.
   - Per-community display name + avatar (initially copied from user defaults).
   - Edit profile page (display name, avatar upload — uploads land in Phase 9; for now accept a URL).
3. [ ] Community shell layout
   - Templ layout `web/templ/layout.templ` with header (community name, member name, logout) and content slot.
   - Nav between `/c/<slug>/chat` and `/c/<slug>/forum`.

**Phase 3 exit:** Logged-in user sees community shell with nav, can edit their per-community profile.

### Phase 4 - Realtime chat channel - status: open

1. [ ] Chat page render
   - `/c/<slug>/chat` renders last 50 messages from SQLite, sanitised markdown, with a send form (templ + datastar reactive form).
2. [ ] Send message handler
   - POST `/c/<slug>/chat/send` — validate, persist, publish rendered templ fragment to NATS subject `community.<id>.chat`.
3. [ ] SSE stream handler
   - GET `/c/<slug>/chat/stream` — opens NATS subscription, forwards fragments as datastar SSE events.
   - On client open via `data-on-load` datastar attribute.
4. [ ] Lazy scrollback
   - "Load older" button (datastar action) fetches page N from DB, prepends.
5. [ ] Markdown pipeline shared
   - `internal/render/markdown.go`: goldmark → bluemonday strict policy → string.
   - Unit tests: `<script>` stripped, fenced code blocks render, links rel=nofollow.
6. [ ] Boundary test
   - Integration: two SSE subscribers + POST send → both receive within 200 ms (use `nats-server` test instance).

**Phase 4 exit:** Two browsers in chat see each other's messages live; refresh shows history; markdown safe.

### Phase 5 - Forum threads & flat+quote replies - status: open

1. [ ] Thread list + create
   - `/c/<slug>/forum` lists threads (subject, author, last reply time).
   - `/c/<slug>/forum/new` form (subject, markdown body).
2. [ ] Thread view + reply
   - `/c/<slug>/forum/<thread_id>` shows opening post + flat replies in time order.
   - Reply form. Optional `quoted_post_id` field; when set, render a quote block above the reply.
3. [ ] Grace-window edit/delete
   - Author may edit/delete own post within 15 min; thereafter mod/admin only.
   - Soft delete: row kept, `deleted_at` set, body replaced with placeholder on render.
4. [ ] Markdown reuse
   - Same render pipeline as chat.
5. [ ] Tests
   - Unit: quote rendering escapes correctly; grace window edge cases.

**Phase 5 exit:** Users can create threads, reply (with optional quote), edit/delete within grace window.

### Phase 6 - Thread → chat bridge - status: open

1. [ ] On thread create, publish system chat message
   - In the same transaction as thread insert, insert a `chat_messages` row with `author_id=NULL`, `kind='thread_announce'`, body `<displayName> started thread: <title>` plus link.
   - After commit, publish rendered fragment to `community.<id>.chat`.
2. [ ] Distinct render for system messages
   - Templ branch on `kind`; different bubble style, link is clickable.
3. [ ] Verify
   - Manual: create thread in one tab, see system message live in another chat tab.
   - Integration test: thread create produces exactly one row with the expected kind.

**Phase 6 exit:** Bridge works one-way as specified.

### Phase 7 - Presence - status: open

1. [ ] In-memory presence map per process
   - Keyed by `(community_id, user_id) → lastSeen`. Heartbeat from chat SSE handler.
2. [ ] Presence broadcast
   - When set changes, publish a small fragment to `community.<id>.presence`; chat sidebar subscribes via SSE.
3. [ ] Heartbeat timeout
   - 30s without heartbeat → drop from list → broadcast update.
4. [ ] Manual verify
   - Two tabs, close one, list updates within timeout.

**Phase 7 exit:** Online list reflects connected users within 30 s.

### Phase 8 - Moderation - status: open

1. [ ] Roles + admin promotion
   - CLI subcommand `app role set <email> admin` (bootstraps first admin).
   - Admin page promotes member ↔ moderator.
2. [ ] Delete actions
   - Mod/admin can soft-delete chat message, post, thread (admin only for thread).
   - Renders placeholder for non-mods; full content for mods (for audit).
3. [ ] Ban
   - Admin: permanent ban. Mod: temp-ban ≤ N days.
   - Session invalidated immediately; login blocked.
   - Banned user's content hidden from non-mods (filter at query layer).
4. [ ] Tests
   - Integration: ban → next request 403; soft-delete visibility differs by role.

**Phase 8 exit:** Moderation actions work and are enforced everywhere content is rendered.

### Phase 9 - Uploads - status: open

1. [ ] Upload handler
   - POST multipart, validate MIME (jpeg/png/webp/gif) + size cap (e.g. 5 MB).
   - Compute sha256, write under `./uploads/<sha256-prefix>/<sha256>.<ext>`.
   - Insert `uploads` row.
2. [ ] Signed-URL middleware
   - GET `/uploads/<id>?sig=…&exp=…` — HMAC of `(id, exp, user_id)`; rejects on mismatch or expiry.
   - On render, generate URL with current user binding.
3. [ ] Embed in chat & forum
   - Drag-drop / file input in chat send form and forum compose; replaces with markdown image link to the signed URL on success.
4. [ ] Tests
   - Boundary: cross-community access rejected; bad MIME rejected; oversized rejected.

**Phase 9 exit:** Image upload works in both chat and forum, with signed-URL serving.

### Phase 10 - Polish, deploy artifacts, smoke - status: open

1. [ ] Error pages + flash messages
   - 404, 403, 5xx templ pages. Flash messages via session.
2. [ ] Production config audit
   - Secure cookie flags, CSRF for state-changing POSTs (chi middleware or `gorilla/csrf`), rate limiting on auth + send endpoints.
3. [ ] Logging + minimal metrics
   - slog JSON in prod, request log middleware, `/metrics` Prometheus endpoint (basic).
4. [ ] Build & ship images
   - CI step (script for now): `make build` → docker image tagged.
   - Document `docker compose up -d` deployment, with `.env.example`.
5. [ ] End-to-end smoke
   - Manual scripted walkthrough: register → verify → login → chat → create thread → see bridge message → reply → mod deletes → ban → upload image. Document outcome in Progress Log.
6. [ ] Update spec Mapping
   - Run `/eidos:spec-refine` (or edit directly) to add `## Mapping` entries to the spec pointing at the actual files for each domain.

**Phase 10 exit:** Spec mapped, deploy artifacts ready, manual smoke passes.

## Verification

- Every spec Behaviour bullet has at least one phase action that produces it.
- Spec Verification list maps to manual or automated checks in the matching phase:
  - Auth flow → Phase 2 tests + Phase 8 ban check.
  - Two-browser chat propagation < 100 ms → Phase 4 manual + integration test.
  - Forum → chat bridge produces exactly one system message → Phase 6 test.
  - Reconnect catch-up → Phase 4 lazy scrollback + manual close/reopen.
  - Moderation visibility → Phase 8 integration test.
  - Presence timeout → Phase 7 manual.
  - Upload cross-community rejection → Phase 9 boundary test.
  - Markdown sanitisation → Phase 4 markdown unit tests.
- Phase 10 end-to-end smoke is the final gate.

## Adjustments

<!-- Document phase shifts, scope changes, or deferrals with timestamp + reason. -->

## Progress Log

- 2606131456 — Plan created from spec via `/eidos:plan`. Phasing: domain-by-domain. Testing: light at boundaries. Deploy: dockerfile + compose from Phase 1.
- 2606131510 — Phase 1 completed on branch `task/phase-1-scaffold`. Scaffold + Dockerfile/compose + migrations + NATS wiring + datastar SSE helper + boot smoke (healthz 200, root templ 200, NATS-down graceful). Module path was already correct (false-positive typo report). Session lib: scs/v2 with memstore for MVP; custom modernc store deferred.
- 2606131520 — Phase 2 completed on same branch. Auth domain + handlers + middleware + CLI (`invite`, `role`, `ban`, `unban`). Unit + integration tests pass. End-to-end HTTP smoke confirms register→verify→login→home→logout→login→bad-login flow. Templ Layout had a `{ children... }` double-render bug (nav + main slots both rendered children) — fixed to single main slot. Service.Register reorders user-insert before invite-consume to satisfy FK on `invite_codes.used_by`.
