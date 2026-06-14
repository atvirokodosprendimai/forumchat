# forumchat

> **Self-hosted community platform in a single Go binary.** Realtime chat,
> a durable forum, WebRTC video rooms, projects with issues, private
> messages, web push, and per-community moderation — one process backed
> by SQLite. AGPL-3.0.

If you've ever wanted **Discord + Discourse + Jitsi + Linear-lite rolled
into one `docker run`**, that's the project. No SaaS lock-in, no SPA
build pipeline, no Kubernetes. Server-rendered HTML over
[Datastar](https://data-star.dev) SSE, ~70 MB image, runs on a $5 VPS or
a Raspberry Pi.

**Built for:** indie hacker communities, study cohorts, family/club
servers, classroom backchannels, open-source project lounges, internal
team spaces, **and freelance / agency client workspaces** — anywhere
a Discord + forum mix would fit but you want to own the data.

> **Each client / company you work with = one community.** Decisions
> live in the forum, day-to-day chatter in chat, deliverables in
> projects (issues + attachments + todos), kickoff calls in the video
> rooms, every file shared via signed upload URLs — all per-client
> siloed under `community_id`. Replace the Slack + Notion + Google Drive
> + Zoom + Loom stack you cobbled together for each engagement with a
> single tab the client can bookmark, and keep the whole history when
> the project ends.

> **Sweet spot:** a solo freelancer or two-person studio with a
> handful of long-term clients — three to ten communities, a few
> contacts each, conversations measured in months not minutes. The
> SQLite + single-binary model is cheap to host and trivial to back
> up (one file), the forum keeps low-frequency context where you'll
> actually find it years later, and per-community push digests stop
> your phone buzzing every time a client types. No seat pricing, no
> tier upsell, no "your free workspace will be archived" emails.

**Pick forumchat if you want:**

- **One binary, one DB file.** SQLite (CGO-free), no Postgres/Redis/queue to babysit. NATS optional.
- **Realtime *and* searchable.** Chat is durable; threads promote into the forum; messages link into todos and bookmarks.
- **Built-in video rooms.** Mesh WebRTC, screen + camera as independent tiles, no Jitsi sidecar.
- **Push notifications that don't spam.** Per-event toggles + 5 / 15 / 60 / 240-min digest mode.
- **Two-step sign-in with magic link.** Email-then-password OR email-me-a-link, anti-enumeration by default.
- **Multi-community by default.** Every row is `community_id`-scoped; public communities discoverable under `/explore`.
- **Boring stack.** Go 1.26 · chi · templ · Datastar · scs sessions · SQLite + goose migrations. No JS framework.

**Try it in 30 seconds:**

```bash
docker run -p 8080:8080 \
  -v $PWD/data:/data \
  -v $PWD/uploads:/uploads \
  -e UPLOADS_DIR=/uploads \
  ghcr.io/atvirokodosprendimai/forumchat:latest
# → open http://localhost:8080 — first user becomes admin
#   data/      → sqlite db + persisted VAPID keys
#   uploads/   → user-uploaded files (kept as a separate folder so backup
#                policies can treat blobs differently from the metadata db)
```

[Quickstart](#local-development) · [What you get](#what-you-get) · [Architecture](#system-architecture) · [Configuration](#configuration) · [Roadmap](#roadmap)

---

## Table of contents

- [What you get](#what-you-get)
- [System architecture](#system-architecture)
- [Tech stack](#tech-stack)
- [Realtime chat](#realtime-chat)
- [Video rooms](#video-rooms-webrtc-mesh)
- [Push notifications + digest mode](#push-notifications--digest-mode)
- [Projects, issues, discussions](#projects-issues-discussions)
- [Auth, sessions, communities](#auth-sessions-communities)
- [Data model](#data-model)
- [Configuration](#configuration)
- [HTTP routes](#http-routes)
- [Project layout](#project-layout)
- [Local development](#local-development)
- [Production notes](#production-notes)
- [Roadmap](#roadmap)

---

## What you get

| Area              | What's there                                                                                                                                                     |
|-------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **Multi-community** | Communities are first-class. Every row is `community_id`-scoped. Public communities appear under `/explore`; private ones are invite-only.                       |
| **Chat**            | One realtime channel per community. Persistent (SQLite), live (NATS pub/sub + datastar SSE), auto-grow textarea composer, mentions, image paste/drop, reply quote. |
| **Forum**           | Threads + flat replies, optional single-parent quote, 15-min self-delete grace, resolved/unresolved filter, search, thread → chat bridge announcement.            |
| **Rooms (video)**   | Eight always-on WebRTC meeting rooms per community. Mesh topology, lazy media policy, screen+camera as independent tiles, background blur, stage fullscreen.     |
| **Projects**        | Optional feature flag. Each project carries discussions, issues (status + comments + attachments), todos, attachments, activity log. Share-link guest viewers.   |
| **Private messages**| Request-based DMs. Recipient must accept before threads open. Live SSE updates + dock toast.                                                                      |
| **Push**            | Web Push (VAPID) with per-community settings, per-event toggles, digest mode (immediate / 5 / 15 / 60 / 240 min).                                                 |
| **Notifications**   | Service worker delivers OS-level pushes; user picks which events wake them and the cadence.                                                                       |
| **Moderation**      | admin / moderator / member roles. Soft-delete with role-aware render. Ban with optional content cleanup window. Trust level reserved for later.                  |
| **Uploads**         | Content-addressed (sha256), HMAC-signed time-bound URLs scoped to viewer + community.                                                                            |
| **Auth**            | Invite-code registration, email verification, cookie sessions (scs), bcrypt cost 12.                                                                              |
| **Bookmarks**       | Per-community, optional folder, attached to chat messages or forum posts.                                                                                         |
| **Todos**           | Per-community, sourced from a chat message or a forum post, status workflow.                                                                                      |
| **History**         | Calendar of community activity; click a date → chronological event list.                                                                                          |
| **Explore**         | Discover public communities; request to join (admin approves).                                                                                                    |

---

## System architecture

The whole server fits in one Go binary. The request side, the realtime side,
and the background workers all share the same process and the same DB.

![Architecture overview](docs/diagrams/architecture.svg)

**Degradation budget**: NATS optional (chat fan-out falls back to single-process
in-memory bus); SMTP optional (`LogMailer` writes URLs to stdout); TURN
optional (mesh-only rooms still work between same-LAN peers); VAPID env values
optional (one-time auto-generation persisted to `./data/vapid.json`).

---

## Tech stack

| Layer            | Choice                                                                |
|------------------|-----------------------------------------------------------------------|
| Language         | Go 1.26 (toolchain)                                                   |
| HTTP router      | `github.com/go-chi/chi/v5`                                            |
| Templating       | `github.com/a-h/templ` (compile-time HTML, all components in `web/templ`) |
| Realtime UI      | [Datastar v1](https://data-star.dev) over Server-Sent Events          |
| Messaging        | NATS core pub/sub (`github.com/nats-io/nats.go`)                      |
| DB               | SQLite via `modernc.org/sqlite` (CGO-free), WAL mode                  |
| Migrations       | `github.com/pressly/goose/v3` (embedded SQL files)                    |
| Markdown         | `yuin/goldmark` + `microcosm-cc/bluemonday`                           |
| Sessions         | `github.com/alexedwards/scs/v2` (in-process memstore)                 |
| Password hash    | bcrypt cost 12                                                        |
| Rate limit       | `github.com/go-chi/httprate`                                          |
| Web push         | `github.com/SherClockHolmes/webpush-go` (VAPID, RFC 8030)             |
| WebRTC           | Browser-native (mesh) — no SFU. Signaling rides the same SSE stream.  |
| Background blur  | MediaPipe Selfie Segmentation v0.1 (lazy-loaded from jsdelivr)        |
| Email (dev)      | SMTP → mailpit (UI at `localhost:8025`)                               |
| Containers       | Distroless `nonroot`, multi-stage Dockerfile                          |
| Frontend         | Inter Variable + JetBrains Mono, OKLCH palette tokens, 2026 design refresh |

---

## Realtime chat

Chat is the closest thing to a global state animation in the system. Persistence
is in SQLite, fan-out across the cluster (or just multiple tabs) is via NATS
core pub/sub, and the UI updates via Datastar over Server-Sent Events.

![Chat realtime flow](docs/diagrams/chat-realtime.svg)

Key design points:

- **Datastar-first rendering**: every UI mutation is a server-rendered HTML
  fragment. The browser does not keep a model of the chat — it morphs a
  `#messages` div from a snapshot the server pushes. Datastar v1 syntax with
  `data-on:` (colon, not hyphen) is used everywhere.
- **NATS optional**: when not connected, the in-process bus still fans out to
  every SSE stream attached to the same process. Useful for tests and tiny
  deployments.
- **Reconnect-safe**: every reconnection of `/chat/stream` triggers a full
  morph of the most-recent 100 messages, so a sleeping tab never gets stuck
  with stale state.
- **Lazy scrollback**: `/chat/older?before=<RFC3339Nano>` returns one batch as
  an SSE prepend.
- **Mentions → push**: `parseMentions` walks the body for `@token` runs,
  resolves them to user IDs through `auth.Repo.UserIDsByDisplayName`
  (case-insensitive), and fires the `PushNotify` closure with
  `kind: "mention"` for any opted-in subscriber.

---

## Video rooms (WebRTC mesh)

Each community owns eight always-on meeting rooms. Whoever joins first becomes
the room admin (rename / public/private toggle / share-link / per-email
invites). Topology is a classic mesh: every participant holds one
`RTCPeerConnection` per other participant, and signaling rides on top of the
same Datastar SSE stream that the page already opened — no second EventSource,
no socket gymnastics.

![Rooms WebRTC mesh](docs/diagrams/rooms-mesh.svg)

Notable choices:

- **Lazy media**: no `getUserMedia` on join. Camera + mic start OFF until the
  user clicks the toggle. Toggling off releases the device (LED off), not
  `.enabled = false`.
- **Independent screen + camera**: enabling screenshare adds a new
  `RTCRtpSender` (`senders.screen`) without replacing the camera sender, so
  viewers see two distinct tiles and can stage either one.
- **Background blur** (default on): the raw camera track is fed into a
  MediaPipe Selfie Segmentation pipeline; the composited canvas is then
  `captureStream(24)`'d and that synthetic track is the one that goes to peers.
  Falls back to raw camera if MediaPipe fails to load. Toggle persists to
  `localStorage`.
- **meta sidecar signal**: stream IDs are stable on the wire but carry no role
  label. A small JSON envelope per peer maps `{streamID → "camera"|"screen"}`
  so receivers paint the right tile chrome (camera tile vs. amber-outlined
  screen tile with 🖥 icon).
- **Stage fullscreen**: dedicated button (top-right of stage), `dblclick`, or
  `f` keypress. Browser-native fullscreen with object-fit contain so screen
  shares stay legible at native resolution.
- **Self-healing TURN**: ICE failures trigger `pc.restartIce()`. ICE candidate
  POSTs are best-effort; the server queues envelopes per recipient so a transient
  re-admission doesn't lose the burst.

---

## Push notifications + digest mode

Web Push is the most architecturally interesting recent addition. It spans the
service worker on the client, a VAPID key pair, a subscription DB, a
fire-and-forget sender, and a polling worker that batches messages for digest
recipients.

### Subscribe flow

![Push subscribe flow](docs/diagrams/push-subscribe.svg)

A few non-obvious bits:

- **`/sw.js` is served from the root**, not from `/static/sw.js`, so the
  service worker can claim scope `'/'`. The handler sets
  `Service-Worker-Allowed: /` for belt-and-braces.
- **VAPID keys**: env values win; otherwise the file at `VAPID_KEYS_FILE`
  (default `./data/vapid.json`) wins; otherwise a fresh pair is generated and
  persisted so reloads keep working. Production should pin via env so a new
  disk doesn't invalidate every browser subscription.
- **Subscriptions are per `(user, community, endpoint)`**: one row per device
  per community. Settings live in the same row as `settings_json` plus
  `digest_minutes` / `digest_last_at` columns.

### Dispatch + digest

![Push dispatch + digest worker](docs/diagrams/push-dispatch-digest.svg)

### Digest mode guarantees

The dropdown on `/c/{slug}/notifications` lets users pick *Immediately / Every
5 / 15 / 60 / 240 minutes*. The contract is intentionally narrow:

- **No silent ticks**. The worker's `DueDigests` query joins
  `push_subscriptions` to `push_pending` and only returns pairs that have
  buffered rows — empty buffer means no notification.
- **No duplicates**. After `BuildDigest` dispatches, the consumed rows are
  deleted from `push_pending` and `digest_last_at` is bumped on every
  subscription for that `(user, community)`. The cooldown restarts uniformly
  across the user's devices.
- **Mixed devices coexist**. A subscription with `digest_minutes = 0` always
  fires immediately. A second subscription on the same user/community with
  `digest_minutes = 5` participates in the digest cycle. Same user, two
  devices, two cadences — fine.

### Decoupling the producers

The chat / forum / projects packages never import the push package. Each one
exposes a `PushNotify` closure field of type:

```go
func(ctx context.Context, communityID, kind string, userIDs []string,
     title, body, url string)
```

`main.go` wires a single closure that adapts the call into either
`pushSender.SendToUsers` (when `userIDs` is non-empty — target mode) or
`pushSender.SendToCommunity` (broadcast). Adding a new event is a one-line
hook call inside the producer + a new toggle on the settings page.

| Event       | Producer             | Mode                                 | Settings key   |
|-------------|----------------------|--------------------------------------|----------------|
| `mention`   | `chat.PostSend`      | target (parsed `@name`s)             | `mention`      |
| `thread_new`| `forum.PostNew`      | broadcast                            | `thread_new`   |
| `project_new`| `projects.PostCreate`| broadcast                           | `project_new`  |
| `report`    | *(reserved)*         | target (mods)                        | `report`       |
| `issue_new` | *(reserved)*         | broadcast                            | `issue_new`    |
| `comment_new`| *(reserved)*        | target (issue subscribers)           | `comment_new`  |

---

## Projects, issues, discussions

Optional feature behind `PROJECTS_ENABLED=true`. When on, each community gets:

- `/c/{slug}/projects` — grid landing
- per-project page with tabs for *Description · Discussions · Issues · Todos
  · Attachments · Activity*
- **Discussions** — forum-style threads scoped to a single project (replies
  with single-parent quote)
- **Issues** — title, body, status (`open`/`in_progress`/`done`/`closed`),
  comments, attachments
- **Share-link guests** — admins mint a time-limited token that lets external
  collaborators view a single project without an account; identity tracking via
  `projects.Identity` keeps writes attributed even for guests

DB tables (numbered by migration): `projects` (00013), `project_issues`,
`project_issue_comments`, `project_issue_attachments` (00014),
`project_discussions`, `project_discussion_replies` (00015).

---

## Auth, sessions, communities

- Registration is **invite-only** unless the database is empty (the
  zero-state bootstrap flow promotes the first user to admin).
- Email verification is mandatory; the `LogMailer` fallback writes the URL to
  stdout for dev convenience.
- Sessions are `alexedwards/scs` v2 with in-process memstore. Cookie name
  `forumchat_session`, max-age driven by `SESSION_MAX_AGE`, `Secure` toggled
  by `ENV`.
- Bcrypt cost **12**; minimum password length 8 (boundary tests in
  `internal/auth`).
- Roles: `member` < `moderator` < `admin`. `RequireRole` middleware ladders.
  Trust level reserved for post-MVP Discourse-style tiers.
- Communities are first-class. Every multi-tenant row carries `community_id`;
  the `LoadCommunity` middleware resolves the URL `{slug}` into a context
  value the handlers read via `community.FromContext`.
- Per-user profile (display name + avatar) writes to **every** membership the
  user holds — the profile editor is "you", not "you in this community".

---

## Data model

17 SQL migrations, embedded via goose and applied on boot (toggle with
`MIGRATE_ON_BOOT`). All tables are `community_id`-scoped where relevant.

| # | Migration                              | What it adds                                                |
|---|----------------------------------------|-------------------------------------------------------------|
| 1 | `00001_init.sql`                       | users, sessions, verification_tokens, communities, memberships, invite_codes, chat_messages, threads, posts, uploads |
| 2 | `00002_invite_queue_and_cleanup.sql`   | pending_users, content-cleanup audit trail                  |
| 3 | `00003_bookmarks.sql`                  | bookmarks                                                   |
| 4 | `00004_thread_resolved.sql`            | resolved flag on threads                                    |
| 5 | `00005_chat_promoted.sql`              | chat → thread promotion link                                |
| 6 | `00006_todos.sql`                      | todos                                                       |
| 7 | `00007_signup_tokens.sql`              | admin-minted signup URLs                                    |
| 8 | `00008_fix_announce_links.sql`         | data fix                                                    |
| 9 | `00009_private_messages.sql`           | pm_threads, pm_messages                                     |
|10 | `00010_community_public.sql`           | community public flag for /explore                          |
|11 | `00011_rooms.sql`                      | rooms, room_pending                                         |
|12 | `00012_rooms_per_community.sql`        | room scoping fix                                            |
|13 | `00013_projects.sql`                   | projects                                                    |
|14 | `00014_project_issues.sql`             | project_issues + comments + attachments                     |
|15 | `00015_project_discussions.sql`        | project_discussions + replies                               |
|16 | `00016_push_subscriptions.sql`         | push_subscriptions                                          |
|17 | `00017_push_digest.sql`                | digest_minutes + digest_last_at + push_pending              |

Key invariants:

- `chat_messages.kind` + nullable `author_id` covers system messages like
  `thread_announce`.
- `chat_messages.promoted_thread_id` carries the chat → forum bridge.
- `memberships.trust_level` reserved for Discourse-style tiers.
- `push_subscriptions` is keyed by `(user_id, community_id, endpoint)` —
  multiple devices per user are just multiple rows.
- `push_pending` rows are deleted after the digest worker drains them, so the
  table stays small.

---

## Configuration

All configuration is via environment variables (or a `.env` file in the
working directory, auto-loaded by godotenv). Defaults are dev-friendly; prod
boot fails fast on placeholder secrets.

### Core

| Variable             | Default                                | Purpose                                                            |
|----------------------|----------------------------------------|--------------------------------------------------------------------|
| `ENV`                | `dev`                                  | `dev` or `prod`. Toggles slog text/json + cookie `Secure` flag.    |
| `HTTP_ADDR`          | `:8080`                                | Listen address.                                                    |
| `BASE_URL`           | `http://localhost:8080`                | Public origin. Used to build verification / invite / push URLs.     |
| `DB_PATH`            | `./data/forumchat.db`                  | SQLite file (auto-creates parent dir).                             |
| `MIGRATE_ON_BOOT`    | `true`                                 | Auto-run goose migrations at startup. Set `false` in prod CI/CD.   |
| `NATS_URL`           | `nats://127.0.0.1:4222`                | NATS connection URL; app degrades gracefully if unreachable.        |
| `SESSION_KEY`        | dev placeholder                        | scs cookie signing key. **Must not contain `dev-only` in prod.**   |
| `SESSION_MAX_AGE`    | `720h` (30 days)                       | Cookie lifetime + idle timeout.                                    |
| `PRESENCE_TTL`       | `30s`                                  | Heartbeat age after which a user drops from the online list.       |
| `EDIT_GRACE`         | `15m`                                  | Window for self-delete of thread / post / chat message.            |
| `COMMUNITY_SLUG`     | `main`                                 | Slug of the bootstrap community.                                   |
| `COMMUNITY_NAME`     | `The Community`                        | Human-friendly name.                                               |

### Uploads

| Variable             | Default                                | Purpose                                                            |
|----------------------|----------------------------------------|--------------------------------------------------------------------|
| `UPLOADS_DIR`        | `./uploads`                            | Local-disk uploads root (content-addressed sha256).                |
| `UPLOADS_MAX_BYTES`  | `5242880` (5 MiB)                      | Per-upload size cap.                                               |
| `UPLOADS_SIGN_KEY`   | dev placeholder                        | HMAC key for signed URLs. **Must not contain `dev-only` in prod.** |

### Email

| Variable             | Default                                | Purpose                                                            |
|----------------------|----------------------------------------|--------------------------------------------------------------------|
| `SMTP_HOST`          | `127.0.0.1`                            | SMTP relay. Set to an unreachable host to force `LogMailer` fallback. |
| `SMTP_PORT`          | `1025`                                 | Mailpit-friendly default.                                          |
| `SMTP_USER` / `PASS` | empty                                  | Optional PLAIN auth.                                               |
| `SMTP_FROM`          | `forumchat@localhost`                  | From header.                                                       |
| `SMTP_TLS`           | `auto`                                 | `auto` / `starttls` / `tls` / `none`.                              |
| `SMTP_TLS_INSECURE`  | `false`                                | Skip cert verification.                                            |

### Rooms (WebRTC)

| Variable             | Default                                | Purpose                                                            |
|----------------------|----------------------------------------|--------------------------------------------------------------------|
| `ROOMS_STUN_URLS`    | `stun:stun.l.google.com:19302`         | Comma-separated STUN URLs.                                         |
| `ROOMS_TURN_URL`     | empty                                  | Optional TURN URL.                                                 |
| `ROOMS_TURN_USERNAME`| empty                                  | TURN auth.                                                         |
| `ROOMS_TURN_PASSWORD`| empty                                  | TURN auth.                                                         |

### Push

| Variable             | Default                                | Purpose                                                            |
|----------------------|----------------------------------------|--------------------------------------------------------------------|
| `VAPID_PUBLIC`       | empty                                  | Override the auto-generated VAPID public key.                      |
| `VAPID_PRIVATE`      | empty                                  | Override the auto-generated VAPID private key.                     |
| `VAPID_SUBJECT`      | `mailto:admin@example.com`             | RFC 8030 subscriber; shown in push-service dispatch logs.          |
| `VAPID_KEYS_FILE`    | `./data/vapid.json`                    | Where auto-generated keys are persisted.                           |

### Features

| Variable             | Default                                | Purpose                                                            |
|----------------------|----------------------------------------|--------------------------------------------------------------------|
| `PROJECTS_ENABLED`   | `false`                                | Mount `/c/{slug}/projects` and show the sidebar link.              |

In production (`ENV=prod`), boot fails fast if `SESSION_KEY` or
`UPLOADS_SIGN_KEY` still contain `dev-only`. Pin `VAPID_*` for prod so a
fresh disk doesn't invalidate every browser subscription.

---

## HTTP routes

Public surface, with most-interesting subsets called out. Auth column:
*none* (public) · *session* (any logged-in user) · *member* (member of the
addressed community) · *mod* / *admin* (role ladder).

### Public

| Method | Path                                | Auth     | Notes                                            |
|-------:|-------------------------------------|----------|--------------------------------------------------|
| GET    | `/healthz`                          | none     | Liveness probe.                                  |
| GET    | `/`                                 | optional | Home (community list when signed in).            |
| GET    | `/register`, `/login`               | none     | Forms.                                           |
| POST   | `/register`, `/login`               | none     | Rate-limited 10/min/IP.                          |
| GET    | `/verify?token=…`                   | none     | Activate + auto sign-in.                         |
| POST   | `/logout`                           | session  |                                                  |
| GET    | `/explore`                          | session  | Public-community discovery.                      |
| GET    | `/profile`                          | session  | Edit display name + avatar URL.                  |
| POST   | `/profile`                          | session  | Writes to every membership the user holds.       |
| GET    | `/messages`                         | session  | PM inbox.                                        |
| GET    | `/messages/{id}`                    | session  | PM thread.                                       |
| GET    | `/messages/stream`                  | session  | Datastar SSE — toasts + badge updates.           |
| GET    | `/sw.js`                            | none     | Service worker (scoped `/`, `Service-Worker-Allowed: /`). |
| GET    | `/push/config`                      | none     | VAPID public key.                                |
| POST   | `/push/subscribe`, `/push/unsubscribe` | session | Subscription lifecycle.                          |
| GET    | `/static/*`                         | none     | Assets (CSS, JS, icons).                          |
| GET    | `/_debug/clock`, `/_debug/clock/stream` | none | Datastar + NATS smoke page.                      |

### Per-community (`/c/{slug}/…`)

| Method | Path                                | Auth     | Notes                                            |
|-------:|-------------------------------------|----------|--------------------------------------------------|
| GET    | `/chat`                             | member   | Last 50 messages.                                |
| POST   | `/chat/send`                        | member   | Persist + fan-out + clear composer + mentions push. |
| GET    | `/chat/stream`                      | member   | Datastar SSE.                                    |
| GET    | `/chat/older?before=<RFC3339Nano>`  | member   | Lazy scrollback.                                 |
| POST   | `/chat/delete?id=…`                 | mod+     | Soft-delete.                                     |
| GET    | `/forum`                            | member   | Threads index.                                   |
| POST   | `/forum/new`                        | member   | Creates thread + chat-announce + `thread_new` push. |
| GET    | `/forum/{id}`                       | member   | Thread + replies.                                |
| POST   | `/forum/{id}/reply`                 | member   | Optional `quoted_post_id`.                       |
| POST   | `/forum/{id}/{resolve,unresolve,rename,delete}` | member | Author/mod/admin gated.                          |
| GET    | `/rooms`                            | member   | Eight-room grid.                                 |
| GET    | `/rooms/{id}`                       | member   | Mesh meeting room.                               |
| POST   | `/rooms/{id}/signal/send`           | member   | Signaling: offer / answer / ice / meta / bye.    |
| GET    | `/rooms/{id}/stream`                | member   | Signaling + chat + admin SSE.                    |
| POST   | `/rooms/{id}/{mic,cam,screen,leave,ping}` | member | Mesh participation.                              |
| POST   | `/rooms/{id}/{public,rename,invite,invite/email,invite/revoke}` | admin | Room admin.                                   |
| GET    | `/notifications`                    | member   | Per-community push settings page.                |
| POST   | `/notifications/save`               | member   | Save toggles + digest interval.                  |
| GET    | `/bookmarks`, `/bookmarks/list`     | member   |                                                  |
| POST   | `/bookmarks`, `/bookmarks/delete`   | member   |                                                  |
| GET    | `/todos`                            | member   |                                                  |
| POST   | `/todos`, `/todos/{id}/{status,delete}` | member |                                                |
| GET    | `/history`                          | member   | Calendar + day events.                           |
| GET    | `/projects[/...]`                   | member or guest | Projects feature (when enabled).            |
| GET    | `/admin`                            | admin    | Community admin.                                 |
| POST   | `/admin/{approve,reject,ban,unban,invite,invite/revoke,add-member,toggle-public}` | admin | |

---

## Project layout

```
cmd/
  app/main.go                  # HTTP server entrypoint + wiring
  cli/main.go                  # forumchat-cli (invite / role / ban / unban)
internal/
  admin/                       # community admin (members, invites, bans)
  auth/                        # users, sessions, mailer, middleware, profile
  bookmarks/                   # bookmarks
  chat/                        # realtime chat (NATS + SSE)
  community/                   # bootstrap + per-request community context
  config/                      # env-driven config + slog setup
  dashboard/                   # signed-in landing (your communities)
  explore/                     # public-community discovery + join requests
  forum/                       # threads, posts, chat bridge
  history/                     # calendar of community activity
  httpx/                       # request logger + recover middleware
  invites/                     # join URLs (admin-minted)
  moderation/                  # report / ban support helpers
  natsx/                       # NATS connect + subject helpers
  presence/                    # in-process tracker + SSE handler
  privatemsg/                  # DM threads + accept/decline + dock toast
  projects/                    # projects, issues, discussions, attachments
  push/                        # Web Push (VAPID), settings page, digest worker
  render/                      # markdown pipeline + image-link wrapper
  rooms/                       # WebRTC mesh (state, signaling, handler)
  storage/sqlite/              # DB open + embedded goose migrations
    migrations/0001-0017       # schema
  todos/                       # per-community todos
  uploads/                     # sha256 store + HMAC signed URLs
web/
  templ/                       # *.templ → generated *_templ.go (gitignored)
  static/                      # app.css, nav.js, push.js, sw.js, rooms.js, rooms-blur.js, paste.js, icons
data/                          # SQLite db + vapid.json (auto-created)
migrations/                    # (legacy mount point; real migrations live under internal/storage/sqlite/migrations)
Dockerfile
compose.yml.example
Makefile
```

---

## Local development

```sh
make tidy          # go mod tidy
make gen           # templ generate
make run           # go run ./cmd/app
```

Without NATS, the app boots fine; chat fan-out is in-process only. To get
realtime fan-out across multiple processes spin up a NATS sidecar:

```sh
docker run -d --name nats -p 4222:4222 nats:2.10-alpine -js
```

For email pickup during dev, start mailpit:

```sh
docker run -d --name mailpit -p 1025:1025 -p 8025:8025 axllent/mailpit
```

Falls back to `LogMailer` when no SMTP is reachable — verification + invite
URLs print to stdout.

### First-time bootstrap

Sign-up is invite-only. After the app is running:

```sh
# Issue an invite code (prints to stdout).
./bin/forumchat-cli invite 1

# Visit http://localhost:8080/register and supply email + password + code.
# Open the verification link (mailpit on :8025 or grep the app log for verify_url).

# Promote yourself to admin.
./bin/forumchat-cli role you@example.com admin
```

Or — if the users table is empty, registering at `/register-as-admin` skips
verification and promotes the first user to admin of the bootstrap community.

### Service worker tip

While iterating on `web/static/sw.js`, the browser caches the SW aggressively.
DevTools → Application → Service Workers → *Update on reload* + *Unregister*
when you change the file. The server already sends `Cache-Control: no-cache`
on `/sw.js`, but the browser keeps the old SW alive until the page closes.

### Make targets

```sh
make tidy   # go mod tidy
make gen    # templ generate (runs *.templ → *_templ.go)
make build  # CGO_ENABLED=0 build into bin/forumchat
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

## Production notes

- Build the image: `docker build -t forumchat:latest .`
- Run behind a TLS terminator; set `ENV=prod`, `BASE_URL=https://your.host`.
- `SESSION_KEY` and `UPLOADS_SIGN_KEY` must be long random strings — boot
  rejects defaults containing `dev-only`.
- **Pin `VAPID_PUBLIC` / `VAPID_PRIVATE`** in prod so a new disk doesn't
  invalidate every browser subscription. The auto-generated `./data/vapid.json`
  is great for dev but a deployment hazard for prod.
- `MIGRATE_ON_BOOT=false` if you prefer running migrations from CI/CD.
- Sessions are in-process (scs memstore) — a restart logs everyone out. For
  multi-instance deployments, swap in a persistent `scs.Store`.
- Uploads live on local disk under `UPLOADS_DIR`. For multi-instance, mount a
  shared volume or replace with an S3-compatible store. The `uploads.Store`
  interface keeps this a focused refactor.
- Email defaults to a single relay; for production set
  `SMTP_HOST/PORT/USER/PASS/FROM`.
- WebRTC needs TURN for symmetric NAT (mobile carriers, corporate, CGNAT).
  Without `ROOMS_TURN_*`, mesh rooms work LAN-only.
- The digest worker runs in the same process. For multi-instance with a
  shared database, run the worker on exactly one instance (a leadership lock
  or a dedicated background pod is a clean way; the worker's queries are
  idempotent under racing tickers but the `digest_last_at` bump may stomp).

---

## Roadmap

The next batch of work, roughly ordered by impact:

- `report` event producer + moderation panel.
- `issue_new` / `comment_new` event producers (the toggles already exist).
- Quiet hours for digest (no notification 22:00–08:00).
- Per-device digest interval (set via subscribe payload, "save this device only").
- Full-text search across chat + forum (SQLite FTS5).
- S3-compatible upload backend.
- Multi-instance push worker with leadership lock.
- JetStream-backed chat with replay on reconnect.
- OAuth (Google → others), linked to existing global identities.
- Drag-drop upload UI.
- Edit history surfaced to users.

---

## License

See `LICENSE`.
