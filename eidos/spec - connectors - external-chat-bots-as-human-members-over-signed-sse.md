---
name: spec-connectors
status: draft
type: spec
tldr: Per-community "external chat bot" connectors — a long-lived, HMAC-signed SSE stream pushes realtime channel messages (JSON, with nick + mentioned flags) to an external worker, and a body-HMAC-signed POST lets it send back. Each connector is backed by a real synthetic member (own user + membership), so it acts as a human (roster, @mentionable, profile) and can join one or many channels. Admin-curated under /c/{slug}/admin/connectors, gated by CONNECTORS_ENABLED. The bidirectional, persistent-stream sibling of webhooks.
---

# Connectors — external chat bots as human members over a signed SSE stream

A connector lets an arbitrary external worker participate in a community's chat
as if it were a person. The worker holds open one **GET SSE stream** to receive
realtime messages and **POSTs** to send them back. You can't know what sits on
the far side — a custom chat app, a chatbot, an e-shop contact widget, a desk
agent — and you shouldn't have to: from the community's point of view it's just
another member typing.

This is **not** webhooks and **not** chat-agents, though it borrows from both:

- **vs webhooks** ([[spec - webhooks - inbound-bot-messages-and-outbound-event-relay]]):
  webhooks are *stateless pushes* — a one-shot inbound `POST` becomes a badged
  *bot* bubble, and an outbound relay fires one JSON `POST` per message at a
  fixed URL. A connector is a *persistent, bidirectional session*: a stream the
  worker keeps open + a send endpoint, and its messages are *human*, not badged.
- **vs chat-agents** (§6.9): an agent runs our *own* model in-process. A
  connector runs *someone else's* logic out-of-process, over the wire.

## Target

The gap: every existing integration surface assumes forumchat owns the logic
(agents) or that the outside world speaks fire-and-forget HTTP (webhooks).
Nothing lets an external program **subscribe to the live conversation and
answer in it as a participant**. That single capability is what you need to
build a support desk, a shop concierge, a bridge to another chat network, or a
bespoke bot — without forumchat knowing or caring which.

"Acts as a human" is the headline: the connector is indistinguishable from a
member, so every feature that already works for members (roster presence,
`@mention`, reply, profile, mod-delete) works for it for free, with no
per-feature special-casing.

## Behaviour

### Feature flag

- New env `CONNECTORS_ENABLED` (bool, default `false`). Read once at boot.
- `false`: `/bots/*` routes not mounted, admin page hidden, no service members
  created. Migration still runs (toggle-on needs no schema work).
- `true`: public `/bots/{id}/stream` + `/bots/{id}/send` mount (outside auth,
  like `/hooks/*`); `/c/{slug}/admin/connectors` mounts.

### Identity — a connector IS a member

On create, the server provisions a **real synthetic member**:

- A `users` row (status `active`, a non-loginable sentinel `password_hash` like
  the OAuth/erasure sentinels, synthetic unique email `connector-<id>@connector.invalid`).
- An **approved** `memberships` row (`role=member`, `approved_at` stamped) in the
  connector's community, `display_name = <connector name>`.
- `connectors.user_id` points at it. The connector's sends are ordinary
  `chat.Service.Send` calls with `AuthorID = user_id` → normal `kind='user'`
  messages. **No bot columns, no badge.** It appears in the roster, is
  `@mention`-able, has a profile, can be replied to, and a mod can delete its
  messages — all the existing member machinery, untouched.

Renaming the connector renames its membership display name; deleting the
connector removes the synthetic member (its authored messages survive as a
"deleted member", same as account erasure — see Friction).

### Channel scope — join one or many

`connector_channels` is a many-to-many allowlist (connector ↔ `chat_channels`):

- The stream delivers **only** messages in the connector's channels; an empty
  allowlist means **all** non-archived community channels.
- A send is rejected (`403`) for a channel outside the allowlist (or archived,
  or another community's) — defence-in-depth even though the URL is the bearer.

### Read — `GET /bots/{id}/stream?exp=<unix>&sig=<hex>`

Public, no session, no CSRF — the **signed URL is the bearer capability**,
exactly like the `/lobby/{token}` guest routes and the shared-signed upload
links ([[internal/uploads/uploads.go]] `SignShared`). Mounted outside the auth
middleware group, behind `httprate` + a heartbeat.

1. `Repo.ByID(id)` → connector (enabled). Miss → **404** (anti-enumeration).
2. Verify `sig = HMAC-SHA256(connector.secret, id || "\n" || "stream" || "\n" || exp)`
   constant-time (`hmac.Equal`); reject expired `exp` (`exp=0` = non-expiring).
   Bad/missing sig → **404** (the URL "doesn't exist"; no oracle).
3. Open a **raw `text/event-stream`** — *not* datastar. This consumer is a
   machine, so the wire is JSON, not HTML fragments.
4. Emit `event: ready` once: `{connector, nick, channels:[…]}` handshake.
5. Subscribe to the in-process chat `Bus` + NATS `ChatSubject` (the same fan-out
   chat itself uses, §6.3a). The Bus carries only the *changed channel id*; the
   stream keeps a per-channel watermark and on each signal loads messages newer
   than it via `chat.Repo.ListAfter(channelID, after, limit)`, emitting one
   `event: message` per new message. Watermark starts at *connect time* —
   v1 is **live-only**, no backlog replay (Future).
6. Per-message filtering: **skip the connector's own messages** (author ==
   `user_id`, no echo), skip soft-deleted, skip `kind='system'`. Deliver
   `user` / `webhook` / `bot` content. If `mentions_only`, deliver **only**
   messages whose body `@mentions` the connector's display name.
7. Heartbeat `:\n\n` comment every ~25 s so proxies don't reap an idle stream.
8. On connect, optionally bump presence so the member shows **online** while the
   worker is attached; clear on disconnect (a small closure, no hard dep).

#### Stream event JSON (`event: message`)

```json
{
  "id": "…", "channel": "support", "channel_id": "…",
  "nick": "alice", "body_md": "hi @Acme can you help?",
  "mentioned": true, "kind": "user",
  "reply_to": "…", "created_at": "2026-06-25T10:00:00Z",
  "attachments": [{"url": "https://…", "mime": "image/png", "name": "x.png"}]
}
```

`nick` is the author's display name; `mentioned` is true when the body
`@mentions` this connector. Both are the "configurable" knobs the worker reads.

### Write — `POST /bots/{id}/send`

Public, signed by **body HMAC** (the tamper-proof path, reusing the GitHub
verification primitive [[internal/webhooks/adapters.go]] `verifyGitHubSignature`).

1. `Repo.ByID(id)` (enabled). Miss → **404**.
2. Read raw body (capped at `CONNECTORS_MAX_BYTES`, default 64 KiB). Verify
   `X-Signature: sha256=<hex>` == `HMAC-SHA256(secret, rawBody)` constant-time.
   Bad/missing → **401**.
3. Parse JSON `{channel, body, reply_to?}`. Resolve `channel` (slug) within the
   community; default to the sole subscribed channel when the body omits it and
   the connector has exactly one. Enforce the allowlist (Channel scope) → `403`.
4. `chat.Service.Send(SendInput{CommunityID, ChannelID, AuthorID: user_id,
   BodyMarkdown: body, ReplyToID})` → a real human message.
5. Fan out (`Bus.Broadcast(channelID)` + NATS `ChatSubject`/`ChatNewSubject`) —
   identical to chat's own send path, so open browser tabs fat-morph it live.
   Connector sends do **not** trigger the outbound-webhook relay (no loop; the
   relay only hooks `chatHandler.PostSend`, which this bypasses).
6. Stamp `last_seen_at`/`last_status`; return `200 {"id": "<msgid>"}`.

### Auth model summary

- `connector.id` is **public** (in the URL). `connector.secret` (32 bytes,
  `crypto/rand`, base64url) is **private**, held only by the worker and the DB.
- **Read = signed URL** (server-minted, revealed once, the worker just opens it).
- **Write = body HMAC** (the worker signs each request; tamper-proof, standard).
- **Rotate** mints a fresh secret → invalidates the stream URL *and* every old
  body signature at once. The single revoke lever.

### Admin CRUD — `/c/{slug}/admin/connectors`

Admin-only (`RequireRole(Admin)`), mirrors `/c/{slug}/admin/webhooks`:

- **Create**: name (nick), avatar URL (optional), channel multi-select
  (checkboxes → CSV signal per §6.7), `mentions_only` checkbox. On save the
  server provisions the member, mints the secret, and **reveals once**: the
  `secret`, the full signed **stream URL**, and the **send URL** + a copy-paste
  `curl`/`EventSource` snippet.
- Per-row: enable/disable, **rotate secret** (re-reveals), edit channels /
  mentions-only, **delete** (removes the synthetic member too), and a
  `last_seen` / `last_status` health column.
- Reached from the `/admin` index (a link), not the left nav — same placement as
  Webhooks / Mail filters.

### Realtime (browser side)

No connector-specific browser stream code: a connector send rides chat's
existing fan-out, so open chat tabs fat-morph the new (human) bubble. The admin
page re-renders its list fragment after each CRUD action via `PatchElementTempl`
on a stable root id (`#connectors-root`, §4.7) — request/response, no SSE.

## Design

### Domain model — new pkg `internal/connectors`

```
connectors
  id            TEXT PK
  community_id  TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE
  user_id       TEXT NOT NULL REFERENCES users(id)               -- synthetic member identity
  name          TEXT NOT NULL                                    -- nick == membership display name
  avatar_url    TEXT NOT NULL DEFAULT ''
  secret        TEXT NOT NULL                                    -- HMAC key (stream sig + body sig)
  mentions_only INTEGER NOT NULL DEFAULT 0
  enabled       INTEGER NOT NULL DEFAULT 1
  created_by    TEXT REFERENCES users(id)
  created_at    INTEGER NOT NULL
  last_seen_at  INTEGER
  last_status   TEXT NOT NULL DEFAULT ''

connector_channels
  connector_id  TEXT NOT NULL REFERENCES connectors(id) ON DELETE CASCADE
  channel_id    TEXT NOT NULL REFERENCES chat_channels(id) ON DELETE CASCADE
  PRIMARY KEY (connector_id, channel_id)

INDEX (community_id)
```

### Package shape (mirrors `webhooks`, AGENTS §6b CQRS)

| File | Role |
|---|---|
| `connectors.go` | Package doc + `Connector` struct + `Repo` (all SQL: `ByID`, `ListForCommunity`, `Create`, `SetEnabled`, `RotateSecret`, `SetChannels`, `Channels`, `Delete`, `Stamp`). |
| `service.go` | Write orchestration: mint secret, provision the member via a `MemberFactory` seam, normalise + persist channels, validation (typed errors). |
| `sign.go` | `StreamSig` / `VerifyStream` (signed URL) + `VerifyBody` (`X-Signature`). Pure, unit-tested. |
| `event.go` | message → wire-JSON encoder + `Mentions(body, nick)` detector. Pure. |
| `stream.go` | `GetStream`: raw JSON SSE loop over the watermark read model. |
| `handler.go` | `PostSend` (public, signed) + admin CRUD. |

### Seams (no import cycles)

- **`MemberFactory`** interface declared in `connectors` (consumer side, §6b):
  `CreateServiceMember(ctx, communityID, displayName, avatar) (userID string, error)`,
  `RenameMember`, `RemoveMember`. Implemented by `auth.Service.CreateServiceAccount`
  (+ rename/remove), wired in main.go — so `connectors` never imports `auth` for
  writes (it still imports `auth` for `FromContext`/roles in the admin handler,
  one-way, like `webhooks` imports `chat`).
- **`chat.Service` + `chat.Repo`** imported directly (one-way), like `webhooks`.
  New read method `chat.Repo.ListAfter(ctx, channelID, after time.Time, limit int)`
  returns messages strictly newer than `after`, chronological — the stream's
  watermark read model.
- **`ResolveAttachments`** closure (reuse the webhooks one) turns upload ids into
  signed URLs for the event JSON — no `uploads` import.
- **Presence bump** closure (optional) — no `presence` import.

### Token & security

- Secret: 32 bytes `crypto/rand`, base64url (~43 chars). Stored plaintext (it IS
  the secret; matches webhooks token + `SESSION_KEY` precedent). Reveal-on-create
  + rotate; no hash-compare needed because comparisons are HMAC-equal, not
  secret-equal.
- Both `/bots/*` routes mounted **outside** session/auth, behind `httprate` and a
  body cap; `/send` additionally requires the body HMAC.
- Unknown id → 404 (anti-enumeration); bad stream sig → 404; bad/missing send
  signature → 401.
- The synthetic member can never log in (sentinel hash) and is auto-approved
  (it's operator-created, not a public signup).

### Config additions

```go
ConnectorsEnabled  bool  `env:"CONNECTORS_ENABLED" envDefault:"false"`
ConnectorsMaxBytes int64 `env:"CONNECTORS_MAX_BYTES" envDefault:"65536"` // 64 KiB
```

### NATS subjects

Reuses `ChatSubject` / `ChatNewSubject` — no new subject. The stream subscribes;
the send publishes. Cross-process realtime works when NATS is up; same-process
works without it (§6.3a).

## Verification

- **sign**: `StreamSig` round-trips; a tampered sig / wrong secret / expired exp
  is rejected; `VerifyBody` is constant-time and rejects a flipped byte. (Pure
  unit tests, no DB.)
- **service**: `Create` provisions a member + approved membership + channel rows
  and a 32-byte secret; `Rotate` changes the secret so an old stream URL stops
  verifying; channel allowlist enforced.
- **send**: unknown id → 404; bad body signature → 401; a valid signed send posts
  as the synthetic member (`kind='user'`, `author_id = user_id`) and a browser
  tab on that channel fat-morphs a *plain* (un-badged) bubble; a send to a
  non-allowlisted channel → 403.
- **stream**: a new human message in a subscribed channel arrives as one
  `event: message` JSON with correct `nick`; the connector's *own* send does not
  echo back; `mentions_only` suppresses a non-mention and delivers a mention with
  `mentioned:true`.
- **acts-as-human**: the connector shows in the member roster and is
  `@mention`-able; a mod can delete its message.
- **flag off**: `/bots/<id>/stream` → 404 route-not-mounted; admin page hidden.
- **smoke**: `make gen && make build && make test`; boot on a fresh high port
  (§13), create a connector in `/admin/connectors`, `curl -N` the stream, post
  from a browser tab, watch the JSON event arrive; `curl` a signed `/send`, watch
  the human bubble appear in the browser.

## Friction

- **Live-only stream (v1).** A reconnecting worker misses messages sent while it
  was away (watermark resets to connect-time). JetStream-backed replay / a
  `?since=` backlog is Future — mirrors the chat replay roadmap item.
- **No outbound retry / delivery guarantee on the stream.** SSE is best-effort;
  if the worker's socket stalls, messages queue in the channel buffer and are
  dropped past capacity (logged). Acceptable for a chat participant.
- **Deleting a connector orphans its authored messages** as a "deleted member"
  (the member row is removed; `chat_messages.author_id` is `ON DELETE SET NULL`).
  Same trade-off as account erasure (§5g). Documented; the history stays readable.
- **Mention detection is best-effort** — a tolerant `@<name>` substring match,
  not a parsed mention token; a display name with spaces or punctuation may
  over/under-match. Good enough for a filter flag; document it.
- **Signed URL in logs.** The stream URL embeds the sig; proxies/CDNs log it.
  Same trade-off as `/lobby/{token}` and `/hooks/{token}`. Rotate mitigates.
- **A connector is a real member**, so it counts toward member counts and roster.
  Intended ("acts as human"), but operators should know connectors inflate the
  headcount.

## Interactions

- Depends on [[spec - chat-channels - admin-curated-public-text-channels]] for the
  per-channel allowlist (`connector_channels` → `chat_channels`).
- Extends [[spec - forumchat - community web app with realtime chat and forum threads]]
  chat write path — but only by *reusing* `chat.Service.Send` as a member; no new
  message kind, no schema change to `chat_messages`.
- Sibling of [[spec - webhooks - inbound-bot-messages-and-outbound-event-relay]]
  (external integration, flag, admin CRUD, signed bearer, NATS fan-out) — the
  persistent-stream, human-identity, bidirectional variant.
- Reuses the synthetic-member precedent from [[spec - social-login - oauth-via-goth]]
  (user provisioning) and the agent-bot sentinel user (§6.9).

## Mapping

> [[internal/connectors/connectors.go]]
> [[internal/connectors/service.go]]
> [[internal/connectors/sign.go]]
> [[internal/connectors/event.go]]
> [[internal/connectors/stream.go]]
> [[internal/connectors/handler.go]]
> [[internal/storage/sqlite/migrations/00073_connectors.sql]]
> [[internal/auth/service.go]]
> [[internal/chat/chat.go]]
> [[web/templ/connectors.templ]]
> [[cmd/app/main.go]]

## Future

- {[ ] Phase 1 — migration + repo + service + `auth.CreateServiceAccount` + sign + tests.}
- {[ ] Phase 2 — public `/bots/{id}/stream` (JSON SSE, watermark) + `/bots/{id}/send` (body HMAC) + `chat.Repo.ListAfter`.}
- {[ ] Phase 3 — admin CRUD page (reveal-once secret + stream/send URL + snippet) + /admin link + main.go wiring + flag.}
- {[?] Backlog replay (`?since=` / JetStream) so a reconnecting worker catches up.}
- {[?] Per-connector send rate limit + monthly quota.}
- {[?] Typed event kinds beyond `message` (member joined/left, channel created) so a bridge can mirror structure.}
- {[?] Outbound delivery log + redelivery (shared with the webhooks retry-queue Future).}
- {[?] Let a connector opt into presence/typing indicators ("Acme is typing…").}

## Notes

### Why a real member instead of a badged bot

Chosen explicitly over the webhook `kind='bot'`/`kind='webhook'` identity: the
brief is "act as a human — you can't know what's on the other side." Backing each
connector with a genuine member means every member feature (roster, `@mention`,
reply, profile, mod-delete) applies with **zero** per-feature code. The cost is a
`users` + `memberships` row per connector and a synthetic email; the benefit is
the connector is a first-class participant, not a second-class notification.

### Why signed-URL read + body-HMAC write (not one mechanism)

A long-lived `EventSource` can't easily send custom headers, so the read path
must carry its credential **in the URL** — a server-minted signed URL is the
right bearer (and reuses `uploads.SignShared`). A write is security-critical and
a plain `POST` *can* carry a header, so it gets the stronger, tamper-proof
**body HMAC** (reusing `verifyGitHubSignature`). One secret powers both; rotating
it revokes both. This pairs the lowest-friction read with the strongest write.

### Why JSON, not datastar

datastar streams exist to morph *our* DOM in *a browser*. A connector's consumer
is arbitrary code that wants data — an e-shop widget, a bridge, a bot. Pushing
HTML fragments would force it to scrape our markup. JSON events keep the contract
stable and the consumer free.
