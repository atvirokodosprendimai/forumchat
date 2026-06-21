---
name: spec-webhooks
status: implemented
type: spec
tldr: Per-community webhooks in two directions — inbound endpoints (token in URL, Discord/Slack style) let external systems POST events that land as bot-identity chat messages via pluggable provider adapters (generic, github), and outbound subscriptions relay community chat activity to an external URL (slack/discord/generic JSON). Admin-curated under /c/{slug}/admin/webhooks, gated by WEBHOOKS_ENABLED.
---

# Webhooks — inbound bot messages & outbound event relay

External systems integrate with a community two ways:

- **Inbound** — GitHub, a CI job, a Matrix hookshot bridge, or any script `POST`s
  to a secret URL and the payload becomes a chat message wearing a per-webhook
  bot name + avatar (Discord-incoming-webhook semantics).
- **Outbound** — new chat messages in a chosen channel are relayed to an
  external `target_url` as a Slack/Discord-compatible (or generic) JSON POST.

This is the **mailbox spec inverted**: where mailbox *pulls* email via IMAP poll
into a per-community surface, webhooks are *pushed*. Same shape otherwise —
feature flag, dedicated `internal/webhooks` package, per-community admin CRUD,
read-model reuse, NATS + in-process Bus fan-out.

## Target

The gap: a community can talk to itself (chat, forum, projects) but nothing
outside can talk to it without a human relaying messages by hand. Every dev
platform (GitHub, GitLab, Sentry, CI, Grafana, Matrix bridges) speaks "outgoing
webhook → JSON POST". Meeting that one wire protocol unlocks all of them at once.

Inbound is the headline ("external system could post to community"); outbound is
the mirror that makes forumchat a participant in a wider notification mesh.

## Behaviour

### Feature flag

- New env `WEBHOOKS_ENABLED` (bool, default `false`). Read once at boot.
- `false`: `/hooks/*` route not mounted, admin page hidden, outbound relay
  subscriber not started. Migration still runs (toggle-on needs no schema work).
- `true`: public `/hooks/{token}` mounts; `/c/{slug}/admin/webhooks` mounts;
  outbound relay subscriber spawns one goroutine per process.

### Inbound — `POST /hooks/{token}`

Public, **no session, no CSRF** — the token IS the bearer secret, exactly like
the existing `/lobby/{token}` guest routes (`cmd/app/main.go`). Mounted outside
the auth middleware group, behind httprate + a body-size cap.

1. `Repo.InboundByToken(token)` → webhook row (`direction='in'`, `enabled=1`).
   Miss → **404** (anti-enumeration; never 401/403 — the URL "doesn't exist").
2. Read body (capped at `WEBHOOKS_MAX_BYTES`, default 1 MiB).
3. If `webhook.secret != ""` **and** the provider signs: verify the signature.
   - `github`: HMAC-SHA256 of the raw body, compared constant-time against
     `X-Hub-Signature-256: sha256=…`. Mismatch → **401**.
4. `adapterFor(provider).Parse(header, body)` → `Rendered{Markdown, Skip}`.
   - `Skip=true` (e.g. GitHub `ping`, empty Slack `text`) → **200**, no message.
5. `chat.Service.PostBot(communityID, channelID, botName, botAvatar, markdown)`
   inserts a `kind='webhook'` message (no `author_id`; bot identity denormalised
   onto the row). `channel_id` is the webhook's configured target.
6. Fan-out: `ChatBus.Broadcast(channelID)` + `ChatNewMsgBus.Broadcast("")` +
   `NATS.Publish(ChatSubject(cid), channelID)` — identical to the forum bridge
   (`internal/forum/handler.go:618`). Stamp `last_at` / `last_status='ok'`.
7. **200**.

#### Provider adapters — `internal/webhooks/adapters.go`

Tiny interface, one method:

```go
type Adapter interface {
    // Parse turns an inbound request into a chat-ready markdown body.
    // Skip=true means "valid but nothing to post" (health pings, empties).
    Parse(h http.Header, body []byte) (Rendered, error)
}
type Rendered struct { Markdown string; Skip bool }
```

- **`generic`** — accepts `{"text": "..."}` OR `{"content": "..."}` (Slack /
  Discord *outgoing* shapes both work) OR, failing JSON, the raw body fenced as
  a code block. This is the Matrix path: a hookshot/maubot bridge fires a
  generic JSON POST. One-way (Matrix → community).
- **`github`** — switches on `X-GitHub-Event`: `push` (→ "N commits to `ref` …"),
  `pull_request` (opened/closed/merged + title + link), `issues`, `release`,
  `ping` (→ Skip). Unknown event → one-line generic fallback ("GitHub: <event>").

Adapters are pure functions of (header, body) — no DB, no I/O — so they unit-test
in isolation with captured fixture payloads.

### Bot identity on `chat_messages`

A `kind='webhook'` message carries its display identity denormalised so the hot
chat read path needs no JOIN to `webhooks`:

- Migration adds `bot_name TEXT NOT NULL DEFAULT ''`,
  `bot_avatar_url TEXT NOT NULL DEFAULT ''` to `chat_messages`.
- New `chat.Kind` `KindWebhook = "webhook"`; new `webtempl.MsgKind` `MsgKindWebhook`.
- `chat.Message` gains `BotName`, `BotAvatar`. The INSERT column list and all
  read scans (`Recent`, `listBefore`, `ByID`) carry the two columns; on scan,
  `kind='webhook'` populates `AuthorName`/`AuthorAvatar` from the bot fields.
- `webtempl.MsgView.IsBot bool` drives the bubble: render avatar + name + body
  like a user bubble, but **suppress author affordances** (PM, profile menu,
  mention, reply-to-author) — a bot has no `author_id`. Bots cannot be promoted,
  bookmarked-to-DM, or @mentioned. Mods can still delete a bot message.

### Outbound — relay subscriber

One process-wide goroutine subscribes to `ChatNewSubject` for every community.
On each "new chat message" signal it loads the just-changed channel's latest
message and, for every `direction='out'` webhook whose `channel_id` is NULL (all
channels) or equals the message's channel, POSTs a JSON body to `target_url`:

- Skips `kind IN ('webhook')` messages → **no echo loops** (an inbound bot post
  must not trigger an outbound relay). Relays human `kind='user'` sends and
  slash-command output (`/resume`, `/prompt` results, which post as
  `kind='system'`) so external integrations hear agent answers too. The chat
  handler's user-send path skips system messages, so the slash path relays
  explicitly from `cmd/app/main.go`.
- Payload shape by provider:
  - `slack` / `discord` → `{"text": "<author>: <body_md>"}` (both consume `text`;
    Discord also accepts it via compat). Channel + author name prefixed.
  - `generic` → `{"community","channel","author","body_md","created_at"}`.
- Delivery is best-effort fire-and-forget with a short timeout; non-2xx stamps
  `last_status` on the row (e.g. `"502"`). **No retry queue in v1** (logged drop,
  documented limitation — see Friction).

### Admin CRUD — `/c/{slug}/admin/webhooks`

Admin-only (`RequireRole(Admin)`), mirrors `/c/{slug}/admin/mail-filters`:

- Two sections: **Inbound** and **Outbound**.
- **Create inbound**: name, avatar URL (optional), target channel `<select>`,
  provider `<select>` (`generic` | `github`), optional signing secret. On save
  the server mints a high-entropy token and renders the full URL
  `<BASE_URL>/hooks/<token>` **once** with a copy affordance.
- **Create outbound**: label, `target_url`, source channel `<select>` (or "all
  channels"), provider `<select>` (`slack` | `discord` | `generic`).
- Per-row: enable/disable toggle, **rotate token** (inbound — invalidates the
  old URL), **delete**, and `last_at` / `last_status` health column.
- The admin page is reachable from the existing `/admin` index (a link), not the
  left nav — same placement decision as Mail filters (#5576).

### Realtime

- Inbound posts use the existing chat fan-out (Bus + NATS) — open chat tabs
  fat-morph the new bot bubble with zero webhook-specific stream code.
- The admin webhooks page re-renders its list fragment after each CRUD action
  via `PatchElementTempl` on a stable root id (no dedicated SSE stream needed;
  CRUD is request/response like mail-filters).

## Design

### Domain model — new pkg `internal/webhooks`

```
webhooks
  id           TEXT PK
  community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE
  direction    TEXT NOT NULL CHECK (direction IN ('in','out'))
  provider     TEXT NOT NULL                 -- in: generic|github ; out: slack|discord|generic
  name         TEXT NOT NULL                 -- bot name (in) / label (out)
  avatar_url   TEXT NOT NULL DEFAULT ''
  channel_id   TEXT REFERENCES chat_channels(id) ON DELETE CASCADE
                                             -- in: target (NOT NULL) ; out: source filter (NULL = all)
  token        TEXT NOT NULL DEFAULT ''      -- in: URL secret (unique when non-empty)
  secret       TEXT NOT NULL DEFAULT ''      -- in: HMAC signing secret (github), optional
  target_url   TEXT NOT NULL DEFAULT ''      -- out: POST destination
  enabled      INTEGER NOT NULL DEFAULT 1
  created_by   TEXT REFERENCES users(id)
  created_at   INTEGER NOT NULL
  last_at      INTEGER                        -- last receipt (in) / delivery (out)
  last_status  TEXT NOT NULL DEFAULT ''
  -- + 2 columns ON chat_messages: bot_name, bot_avatar_url
UNIQUE INDEX (token) WHERE token <> ''
INDEX (community_id, direction)
```

### Package shape (mirrors `chat`/`forum`/`mailbox`, AGENTS §6b CQRS)

| File | Role |
|---|---|
| `repo.go` | All SQL: `InboundByToken`, `ListForCommunity`, `OutboundForChannel`, `Create`, `SetEnabled`, `RotateToken`, `Delete`, `StampDelivery`. |
| `service.go` | Write orchestration: mint token (crypto/rand), validate provider×direction, normalize. |
| `adapters.go` | Inbound provider parsers (pure funcs). |
| `relay.go` | Outbound subscriber goroutine + per-provider payload encoder. |
| `handler.go` | `PostInbound` (public) + admin CRUD handlers. Imports `chat` directly (one-way, like `forum`). |

`handler.go` holds `Chat *chat.Service`, `ChatBus *chat.Bus`,
`ChatNewMsgBus *chat.Bus`, `NATS *nats.Conn`, `Communities *community.Repo` —
the exact field set `forum.Handler` already carries. No new interface ceremony.

### Token & security

- Token: 32 bytes from `crypto/rand`, base64url → ~43 chars. Stored plaintext
  (the URL is the secret; matches the codebase's `/lobby/{token}` + `SESSION_KEY`
  plaintext-secret precedent). Reveal-on-create + rotate; no hash-compare.
- `/hooks/{token}` is mounted **outside** session/auth middleware in its own
  route group, behind `httprate` (e.g. 60/min per IP) and a 1 MiB body cap.
- GitHub HMAC verification is the only signature path in v1; constant-time
  compare via `hmac.Equal`.
- Outbound `target_url` is admin-supplied; document SSRF risk (admins are
  trusted; no internal-network allowlist in v1 — Friction).

### Config additions

```go
WebhooksEnabled  bool  `env:"WEBHOOKS_ENABLED" envDefault:"false"`
WebhooksMaxBytes int64 `env:"WEBHOOKS_MAX_BYTES" envDefault:"1048576"` // 1 MiB
```

### NATS subjects

Reuses existing `ChatSubject` / `ChatNewSubject` — no new subject. Inbound posts
ride chat's fan-out; outbound relay subscribes to `ChatNewSubject`.

## Verification

- **Inbound generic**: `curl -d '{"text":"hi"}' /hooks/<token>` → bot bubble
  "hi" appears live in the target channel of an open tab.
- **Inbound github**: POST a captured `push` fixture with a valid
  `X-Hub-Signature-256` → formatted commit summary bubble. `ping` → 200, no
  message. Bad signature → 401.
- **Anti-enum**: `POST /hooks/does-not-exist` → 404 (not 401/403).
- **Bot identity**: bot bubble shows webhook name+avatar, exposes no PM/profile
  menu, cannot be @mentioned; a mod can delete it.
- **No echo loop**: an inbound bot post does NOT fire an outbound relay.
- **Outbound**: configure a `generic` outbound at a mock server; a human chat
  message in the source channel arrives as a JSON POST with `body_md`; a
  `kind='webhook'` message does not.
- **Adapters** unit-tested against fixture payloads (no DB).
- **Flag off**: `/hooks/<token>` → 404 route-not-mounted; admin page hidden.

## Friction

- **No outbound retry queue (v1).** A flaky `target_url` drops the relay (logged,
  `last_status` stamped). JetStream-backed retry is future, mirrors the chat
  replay roadmap item.
- **Outbound SSRF.** `target_url` is admin-controlled and unrestricted; a
  malicious admin could point it at an internal address. Admins are already
  trusted (they can delete the community). Allowlist is future.
- **Token in URL.** Logged by proxies/CDNs. Same trade-off the codebase already
  accepts for `/lobby/{token}`. Rotate mitigates leak.
- **Bot identity is denormalised**, not live-joined — renaming a webhook does not
  retro-rename its old messages. Acceptable (Discord behaves the same).
- **chat_messages read-path change.** Two columns added to every chat scan; the
  hot path stays JOIN-free but every scan site must carry the new columns (the
  one place this feature touches existing code; covered by existing chat tests).

## Interactions

- Depends on [[spec - chat-channels - admin-curated-public-text-channels]] for
  the per-channel target/source (`channel_id`).
- Extends [[spec - forumchat - community web app with realtime chat and forum threads]]
  chat write path with `kind='webhook'` + bot identity columns.
- Mirrors [[spec - mailbox - imap-ingest-to-per-community-queue]] structurally
  (external ingest, flag, admin CRUD, read-model reuse) — inverted (push vs poll).

## Mapping

> [[internal/webhooks/repo.go]]
> [[internal/webhooks/service.go]]
> [[internal/webhooks/adapters.go]]
> [[internal/webhooks/relay.go]]
> [[internal/webhooks/handler.go]]
> [[internal/storage/sqlite/migrations/00042_webhooks.sql]]
> [[internal/chat/chat.go]]
> [[web/templ/webhooks.templ]]
> [[cmd/app/main.go]]

## Future

- {[x] Phase 1 — migration (webhooks table + chat_messages bot columns) + repo + chat.PostBot + KindWebhook render.}
- {[x] Phase 2 — inbound `/hooks/{token}` + generic & github adapters + admin create/list/delete inbound.}
- {[x] Phase 3 — outbound relay (chat.Handler.RelayOut hook) + slack/discord/generic encoders + admin CRUD.}
- {[?] Outbound JetStream retry queue with delivery log.}
- {[?] Per-provider inbound suite (GitLab, Sentry, Grafana, …).}
- {[?] True Matrix Application Service bridge (bidirectional, federation, puppeting) — separate spec, not a webhook.}
- {[?] Outbound `target_url` SSRF allowlist.}
- {[?] Signed outbound (HMAC of body so receivers can verify forumchat).}

## Notes

### Why bot identity instead of a system message

The user chose Discord-style bot identity over author-less system messages: each
webhook posts under its own name + avatar so a channel with a GitHub hook and a
CI hook reads cleanly. Cost is the `chat_messages` schema touch; benefit is the
integrations are visually distinguishable and self-documenting.

### Why generic + github (not a full provider suite)

Generic covers everything that can be configured to POST `{text}`/`{content}` —
Slack outgoing, Discord, Matrix hookshot, most CI. GitHub gets a dedicated
adapter because its payloads are rich and ubiquitous and a raw-JSON dump would be
unreadable. More providers are additive (one `case` in `adapterFor`) — no
architecture change.

### Matrix decision

Treated as a `generic` inbound source via its existing webhook bridges
(hookshot/maubot). A native Matrix bridge is explicitly deferred — it is an
Application Service (federation, room puppeting), an order of magnitude larger
than this feature, and warrants its own spec.
