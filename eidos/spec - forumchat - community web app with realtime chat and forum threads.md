---
tldr: Community web app combining a single realtime chat channel with a forum (threads + flat replies + quote), built in Go with datastar SSE, NATS core pub/sub, SQLite, and templ.
---

# forumchat

A web-based community space that blends the *moment* of Discord/Telegram chat with the *memory* of a classic forum. One realtime chat channel keeps people present; forum threads keep durable conversation. Every new thread announces itself in the chat channel as a system message so the community never misses topics worth discussing.

Designed single-community now, multi-community-ready in the data model.

## Target

- Give a small-to-medium community **one place to gather** without forcing them to choose between live chat and durable discussion.
- Avoid the fragmentation of DMs — communication is **public to the community by design**.
- Keep operations cheap and deployable to a single box (SQLite + local disk + embedded NATS or sidecar).
- Reach a working MVP without OAuth, SaaS storage, or external email infra (beyond an SMTP relay for verification).

## Behaviour

### Identity & access

- Account is **global** (one email = one user across the system) but **profile is per-community** — display name, avatar, role may differ per community.
- Registration is **invite-code + email verification**:
  - User submits email, password, and an invite code.
  - Account is created in `pending` state; verification email sent.
  - Clicking the link activates the account.
  - Unverified accounts cannot post or chat; they may not be visible in member lists.
- Login is **email + password** via cookie session.
- OAuth (Google, Facebook) is **out of MVP** but the user/identity table reserves room for linked external identities.
- Sessions are server-side cookies; signing key in env.

### Communities

- MVP ships with **one community** ("the community") created at bootstrap.
- Data model carries a `community_id` on every membership, message, thread, post, role, ban — so adding a community list/picker later is a UI + routing change, not a migration.
- Joining = becoming a member of a community. In single-community mode, every verified user is auto-joined.

### Chat channel (one per community)

- One persistent chat channel per community.
- Messages are **stored in SQLite** (infinite scrollback, lazy-loaded on scroll-up).
- Messages are **broadcast via NATS core pub/sub** to all connected datastar SSE clients in that channel.
- On reconnect, the client requests the last *N* messages from the server via HTTP (catch-up from DB), then resumes live via SSE — **NATS itself does not replay**.
- Markdown body, rendered server-side via goldmark, sanitised with bluemonday.
- Image attachments allowed (see uploads).
- System messages (thread auto-posts, bans, etc.) render differently from user messages.

### Forum (threads + replies)

- Anyone (verified, not banned) can **create a thread** with a subject and a markdown body.
- Replies are **flat in time order**, with optional **single-parent quote** of any earlier post in the same thread.
- Each thread has a stable URL.
- Editing own post within a grace window (e.g. 15 min); admin/mod can edit any time. Edit history is not surfaced in MVP.
- Deleting a post by author within grace window, otherwise mod/admin only.

### Thread ↔ chat link (the bridge)

- When a thread is created, the server publishes a **system message** to the community chat channel:
  > `<displayName> started thread: <title>` *(link)*
- The chat message is durable (stored in SQLite), looks visually distinct from user messages, and includes a clickable title that navigates to the thread.
- Replying in the thread does **not** echo back to chat. The bridge is one-way at creation only.

### Presence

- Per-community **online member list**, updated in realtime.
- Presence is "connected to a chat SSE stream within the last *T* seconds".
- Implemented as an in-memory map per server process; if multi-process later, switch to NATS KV with TTL.

### Moderation

- Roles: `admin`, `moderator`, `member`. **Trust levels** (Discourse-style) are reserved for post-MVP but the schema includes a `trust_level` column so promotions don't require a migration.
- `admin` can: assign moderators, delete any post/message/thread, ban users, manage invite codes.
- `moderator` can: delete any post/message/thread, temp-ban users (≤ N days).
- Deletion is **soft** (row kept, marked deleted, body replaced with placeholder on render).
- Ban: banned user's session is invalidated; subsequent logins blocked; their content is hidden from non-mods.

### Uploads

- Image uploads attach to chat or forum posts.
- Stored on **local disk** under `./uploads/` with a content-addressed path (sha256-prefixed).
- Served via signed-URL middleware that checks the requesting user's community membership.
- Size cap and MIME allow-list enforced.

### What this is NOT

- **No DMs.** Communication is community-public by design.
- **No multiple chat channels per community** — exactly one.
- **No nested reply trees.** Flat + quote only.
- **No federation.**

## Design

### Stack

- **Language**: Go (`go 1.25`).
- **Router**: `chi`.
- **Sessions**: server-side cookie sessions (gorilla/sessions or `alexedwards/scs`).
- **Templating**: `a-h/templ`.
- **Realtime UI**: [datastar](https://data-star.dev) over SSE; reactive signals on the client.
- **Messaging**: NATS core pub/sub (no JetStream in MVP).
- **DB**: SQLite via `modernc.org/sqlite` (CGO-free) — schema migrations via `golang-migrate` or `pressly/goose`.
- **Markdown**: `yuin/goldmark` + `microcosm-cc/bluemonday`.
- **Storage**: local disk for uploads.
- **Email**: SMTP relay (config-driven) for verification.

### Process shape

```
browser  <--HTTP/SSE--  forumchat (Go) ---> SQLite
                              |
                              +-- NATS (pub/sub) <--> (future: other forumchat instances)
```

A single Go binary serves HTML pages (templ), an SSE endpoint per page-channel, and JSON/form-post action endpoints. NATS is local (embedded or sidecar) for MVP and exists primarily so adding a second process later is straightforward.

### Realtime data flow

1. Client opens a page (e.g. `/community/<slug>/chat`). Server renders templ, including a datastar `data-on-load` that opens an SSE stream to `/community/<slug>/chat/stream`.
2. The SSE handler subscribes to NATS subject `community.<id>.chat`.
3. When a user posts, the action handler:
   - validates + sanitises,
   - persists to SQLite,
   - publishes a rendered fragment (templ → string) to `community.<id>.chat`.
4. Every SSE handler forwards NATS messages as `datastar-merge-fragments` events; browsers patch the DOM.
5. Same pattern for thread events (`community.<id>.forum.threads`) and presence (`community.<id>.presence`).

> Server-rendered fragments over the wire keep the client dumb. No JSON-to-DOM logic on the browser.

### Schema sketch (SQLite)

```
users(id, email UNIQUE, password_hash, status, created_at, ...)
communities(id, slug UNIQUE, name, created_at)
memberships(user_id, community_id, display_name, avatar, role, trust_level, banned_until, ...)
invite_codes(code, community_id, created_by, used_by, used_at, expires_at)
chat_messages(id, community_id, author_id NULL, kind, body_md, body_html, deleted_at, created_at)
threads(id, community_id, author_id, subject, body_md, body_html, deleted_at, created_at)
posts(id, thread_id, author_id, quoted_post_id NULL, body_md, body_html, deleted_at, created_at)
uploads(id, owner_id, sha256, mime, size, created_at)
sessions(...)  -- managed by session lib
```

`author_id NULL` + `kind` on `chat_messages` covers system messages (e.g. `kind='thread_announce'`).

### Project layout (proposed)

```
cmd/app/main.go              -- entrypoint, wiring
internal/auth/                -- registration, login, sessions, invites
internal/community/           -- community + membership domain
internal/chat/                -- chat domain, NATS publisher, SSE handler
internal/forum/               -- threads + posts
internal/presence/            -- presence map, heartbeat
internal/moderation/          -- delete/ban actions
internal/uploads/             -- write, serve, sign
internal/render/              -- templ components, markdown pipeline
internal/storage/sqlite/      -- migrations, queries
internal/nats/                -- conn + subject helpers
web/                          -- templ files, static assets
```

## Verification

- **Auth**: invite code required, verification email is sent and required, wrong code or expired link blocks activation. Banned user cannot log in.
- **Chat**: two browsers connected to the same channel; a message posted in A appears in B within ~100 ms without page reload.
- **Forum → chat bridge**: creating a thread produces exactly one system message in the channel, with a working link.
- **Reconnect**: closing the SSE tab, reopening, and seeing prior messages already rendered (DB catch-up) plus new messages live.
- **Moderation**: mod-deleted message disappears for non-mods, remains placeholder for mods; banned user's session ends on next request.
- **Presence**: closing tab removes user from list within heartbeat timeout.
- **Uploads**: cannot fetch another community's upload URL; over-size or wrong-MIME upload rejected.
- **Markdown**: posted `<script>` tag is sanitised; goldmark renders standard syntax; signed-URL images render in-line.

## Friction

- **NATS core pub/sub has no replay.** A burst of disconnections during a chat spike means missed messages until the DB catch-up call — acceptable for MVP, but a user-visible gap. JetStream would fix this; deferred to keep MVP small.
- **Presence in-process** means horizontal scaling later requires switching to NATS KV (or another shared store) — flagged in [[Future]].
- **Local-disk uploads** prevent multi-instance deployment without shared storage.
- **Single community at MVP** but full schema overhead of `community_id` everywhere — paid now to avoid migration later.
- **Email verification** requires SMTP config out of the box; local dev needs a maildev/mailpit container.
- **No edit history** in MVP — a moderation surprise if abused.
- **Markdown sanitisation** is conservative; some users will want richer formatting (tables, images-by-url) and complain.

## Future

- {[!] OAuth: Google then Facebook, linked to existing global user.}
- {[!] Multi-community UI: community picker, per-community navigation, invite per community.}
- {[!] JetStream-backed chat with replay on reconnect.}
- {[?] Trust levels and per-trust-level capabilities (Discourse-inspired).}
- {[?] Push/web notifications for thread replies.}
- {[?] Search across chat + forum (FTS5).}
- {[?] S3-compatible upload backend for horizontal scaling.}
- {[?] Edit history surfaced to users.}

## Notes

- Treat the chat channel as the **heartbeat** and the forum as the **memory**. Every design tradeoff should favour that split.
- "datastar + NATS + Go" is the chosen ergonomics; resist replacing with React/JSON-API unless a concrete need emerges.
- The bridge (thread → system message in chat) is the central feature distinguishing this from "yet another forum" or "yet another chat".
