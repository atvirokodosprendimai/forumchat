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

### Phase 1 - Project scaffold & dev environment - status: open

1. [ ] Initialise git repo and fix `go.mod` module path
   - `git init`, base `.gitignore` (binaries, `./uploads/`, `*.db`, `tmp/`).
   - Confirm intended module path (likely `github.com/atvirokodosprendimai/forumchat`); fix typo `gihtub` → `github` if confirmed.
   - Initial commit "chore: init repo".
2. [ ] Create directory skeleton per spec
   - `internal/{auth,community,chat,forum,presence,moderation,uploads,render,storage/sqlite,natsx}` (note: `nats` is a stdlib-conflicting name in some IDEs — use `natsx`).
   - `web/` with `web/templ/` and `web/static/`.
   - `migrations/` for goose/golang-migrate SQL.
3. [ ] Add core deps and `tools.go`
   - `chi`, `templ`, `datastar`, `nats.go`, `modernc.org/sqlite`, `goldmark`, `bluemonday`, session lib (see action 5).
   - `tools.go` build-tag for `templ` CLI and `goose`.
   - `go mod tidy`.
4. [ ] Decide session library
   - Run `/eidos:decision session library — alexedwards/scs vs gorilla/sessions`.
   - Capture tradeoffs (revocation ergonomics, SQLite store option, middleware fit).
5. [ ] Wire `cmd/app/main.go` minimum
   - chi router, health endpoint `/healthz`, templ "hello" page, env-driven config (port, DB path, NATS URL, SMTP).
   - Structured logger (slog).
   - `go run ./cmd/app` serves the hello page.
6. [ ] Dockerfile + docker compose
   - Multi-stage Dockerfile (golang build → distroless or alpine runtime).
   - `docker-compose.yml` services: `app`, `nats`, `mailpit` (SMTP capture for dev), volume for `./uploads` and `./data` (SQLite).
   - `make dev` / `make up` shortcut.
7. [ ] SQLite + migrations bootstrap
   - Pick migration tool (default goose; record in action note).
   - `migrations/0001_init.sql` creates the `users`, `communities`, `memberships`, `invite_codes`, `chat_messages`, `threads`, `posts`, `uploads` skeleton from the spec sketch (incl. `community_id` everywhere, `trust_level` column).
   - App auto-runs migrations on startup behind a `MIGRATE_ON_BOOT=true` env (off in prod).
8. [ ] NATS wiring
   - Connect on boot with reconnect, log on disconnect.
   - Subject helpers: `subjects.ChatCommunity(id) string` etc.
   - Smoke handler: POST `/_debug/publish` (dev-only) publishes; SSE `/_debug/stream` subscribes — verify round-trip in two tabs.
9. [ ] Datastar SSE base helper
   - Reusable `render.SSEStream(w, ctx, fn)` that flushes templ fragments with `datastar-merge-fragments` events.
   - Smoke page: time-of-day ticker using datastar — verify in browser.

**Phase 1 exit:** `make up` boots app + NATS + mailpit; smoke pages prove datastar+NATS round-trip; migrations create schema; module path correct.

### Phase 2 - Auth & invite registration - status: open

1. [ ] Auth domain shape
   - `internal/auth/` with `Service`, `User`, `Session` types, repository over SQLite.
   - Password hashing: `bcrypt` (cost 12).
2. [ ] Invite code lifecycle
   - Admin-only command (CLI subcommand `app invite create`) generates code into `invite_codes`.
   - Codes have `expires_at`, `used_by`, `used_at`.
3. [ ] Registration flow
   - GET `/register` (templ form: email, password, invite code).
   - POST `/register` validates invite, creates user `status=pending`, generates verification token (separate `verification_tokens` table — add migration).
   - Sends email via SMTP to mailpit; verify visible in mailpit UI.
4. [ ] Email verification
   - GET `/verify?token=…` activates user, auto-joins single community (`memberships` row with `role=member`).
5. [ ] Login + sessions
   - GET `/login`, POST `/login`, POST `/logout`.
   - Session cookie via chosen lib; banned users blocked at login + on every request (middleware checks `banned_until`).
6. [ ] Auth middleware + context
   - `RequireAuth`, `RequireRole(role)`, attaches user + active membership to request context.
7. [ ] Tests (boundary)
   - Unit: password hash, invite consume idempotency.
   - Integration: httptest of full register→verify→login→logout cycle, mailpit SMTP optional via testcontainers or a local fake.

**Phase 2 exit:** Can register with invite, verify via mailpit, log in, log out, banned user is blocked.

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
