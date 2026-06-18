---
name: spec-chat-channels-admin-curated-public-text-channels
status: draft
type: spec
tldr: Split the single community chat into multiple admin-curated, all-public named text channels (Slack/Discord style). Today's chat becomes an undeletable #general; admins/mods add up to ~10 more. Members read+write every channel; one switcher bar sits above the message list. No coupling to projects, forum, or video rooms.
---

# Chat Channels — admin-curated public text channels

Today `internal/chat` is one community-wide stream: every message carries a `community_id` and every member sees the same fat-morphed `#messages` list. For a community of 5-10 people running a few projects, one undifferentiated channel gets noisy — design chatter, off-topic, and per-project talk all pile into the same scroll. This spec adds lightweight **named text channels** as scopes *within* the chat surface, while keeping the current single-channel experience as the zero-config default.

## Target

Give small communities (5-10 members, a handful of projects) a way to separate chat into a few topic scopes without standing up a new surface or learning a new mental model. The default (one channel) must keep working untouched; channels are purely additive. This is **not** project-discussions (forum-style threads per project), **not** video `rooms` (WebRTC), and **not** forum threads — it's the realtime chat, scoped.

## Behaviour

- The community chat gains a **channel switcher** bar above `#messages`. It lists the community's channels; the active one is highlighted.
- Selecting a channel fat-morphs `#messages` to that channel's latest 100 messages and points the composer at it. URL reflects the active channel (`/c/{slug}/chat/{channelSlug}`); refresh/deep-link lands on the same channel.
- **#general always exists and can't be deleted or archived.** It's the landing channel; the bare `/c/{slug}/chat` redirects to it.
- **All channels are public.** Every member reads and writes every non-archived channel — no per-channel membership, no join step. (See [[c - all-public channels need no membership table]].)
- **Only admins/mods create, rename, set topic, reorder, or archive channels.** Members only pick and post.
  - Soft cap of ~10 channels per community: the create UI warns past the cap; the server hard-rejects to keep the switcher tidy and the per-channel fan-out cheap.
  - Archive (not hard-delete) for non-default channels: archived channels drop out of the switcher, keep their history as read-only, and free the name for reuse. Hard-delete is admin-only and cascades messages.
- **Realtime is per-channel.** A message sent to channel A morphs A's viewers' `#messages`; it does **not** disturb someone reading channel B. But B's switcher shows an **unread dot** on A so nothing is missed.
- Everything chat already does carries over **per channel**: replies/quotes, attachments (multi-MIME), promote-to-thread, extract-to-project, soft-delete + `[message removed]` placeholder, read receipts, mod delete, rate limiting. None of these gain a channel concept beyond scoping by `channel_id`.
- Migration backfills: every existing community gets a `general` channel and every existing `chat_messages` / `chat_reads` row is stamped with it. Zero data loss, no visible change for a community that never makes a second channel.

### Permissions

| Action | Member | Mod | Admin |
|---|:--:|:--:|:--:|
| Read / post in any non-archived channel | ✓ | ✓ | ✓ |
| Create / rename / set topic / reorder | — | ✓ | ✓ |
| Archive non-default channel | — | ✓ | ✓ |
| Hard-delete channel | — | — | ✓ |
| Delete/archive #general | — | — | — |

## Design

Follows the codebase's CQRS-ish split (§6b of AGENTS.md) and the fat-morph + "publish the id, refetch on the other side" pattern (§6). The active-channel selection is the **one** new client signal; the rest is channel-scoping existing queries.

### Data model

New migration `00032_chat_channels.sql`:

- `chat_channels(id, community_id FK, slug, name, topic, position INT, is_default INT, archived_at INT NULL, created_by FK, created_at)`, `UNIQUE(community_id, slug)`.
- `ALTER TABLE chat_messages ADD COLUMN channel_id TEXT REFERENCES chat_channels(id) ON DELETE CASCADE`.
- `ALTER TABLE chat_reads ADD COLUMN channel_id TEXT` — read receipts become per-channel so unread is per-channel.
- Backfill in the same migration: insert one `general` (`is_default=1`, `position=0`) per community, then `UPDATE chat_messages / chat_reads SET channel_id = <that general's id>` for each community. Mind the modernc FK ordering trap (§8): insert channels before the UPDATEs.
- New index `idx_chat_messages_channel_created ON chat_messages(channel_id, created_at)`; the old `(community_id, created_at)` index can stay or be dropped — channel reads key on the new one.

### Read side (queries)

- `chat.Repo.Recent` / `Before` / `MarkRead` / `ReadersSince` gain a `channelID` param and key on `channel_id` instead of `community_id`. `community_id` stays on the row for auth/ownership only.
- New `Repo.ListChannels(ctx, communityID, includeArchived)` and `Repo.ChannelBySlug(ctx, communityID, slug)`.
- New `Repo.UnreadChannels(ctx, communityID, userID)` → set of channel ids with messages newer than the viewer's per-channel `last_read_at`, to seed unread dots on page load.
- The read model is still one pure function `(channelID, viewer) → latest-100 → MessagesContainer templ`, called from both page load and the SSE loop (§6b). It just takes a channel id now.

### Write side (commands)

- `chat.Service.Send` / `PostSystem` gain a `ChannelID`; validation rejects sends to an archived or foreign-community channel.
- New `chat.Service` channel ops (admin/mod-gated in the handler, single-writer in the service): `CreateChannel` (slugify name, enforce ~10 cap, `position = max+1`), `RenameChannel`, `SetTopic`, `Reorder`, `Archive`, `Delete` (refuse on `is_default`).
- Forum→chat bridge (`thread_announce`) posts into **#general** by default; promote-to-thread keeps the originating message's `channel_id`.

### Realtime — keep one SSE stream, carry the channel id on the wire

Don't open a new EventSource per channel switch. Keep the existing single community-wide chat stream and make the **payload the channel id** (§6.4 / §6b wire-payload guidance):

- Publishers broadcast the changed `channelID` to `chat.Bus` and NATS subject `community.<cid>.chat` (payload = channel id, not `"changed"`).
- `Handler.GetStream` holds the viewer's `$active_channel`. On an event:
  - if `payload == active_channel` → refetch that channel's latest 100 and fat-morph `#messages` + scroll (the existing flow), and clear that channel's unread dot.
  - else → set the unread-dot signal for `payload`'s channel; **don't** touch `#messages`.
- Switching channel is a client action: `data-on:click` sets `$active_channel`, the handler's switch endpoint (`@get('/c/{slug}/chat/{slug}')`) fat-morphs `#messages` to the new channel, pushes the URL, marks-read, and clears the dot. The long-lived stream stays open across switches — no resubscribe churn.

> Alternative considered: per-channel Bus keyed by channel id (§4.11), one EventSource re-opened per switch. Rejected — it loses the cross-channel unread dots for free, and resubscribe flicker on every switch is worse UX than a single stream filtering by id. The per-channel Bus is the right call when viewers only ever watch one row (lobbies); here a viewer cares about *all* channels' unread state at once.

### UI (datastar)

- `web/templ/chat.templ`: a `ChannelSwitcher(channels, activeSlug, unread, isMod)` component above `#messages`. Pills/tabs, horizontal-scroll on mobile. Each pill: name, optional unread dot (`data-show="$chat_unread[id]"`), `data-on:click="$active_channel='<id>'; @get('/c/{slug}/chat/<slug>')"`.
- Mod/admin get a `+ channel` affordance and a per-channel ⚙ (rename / topic / archive). Reuse the EDA dispatch pattern (§4.12) — one `data-on:fc:channel-edit` consumer, N producer buttons — rather than N inline handlers.
- New signals in `layout.templ` `InitialSignals`: `active_channel` (string), `chat_unread` (object/map id→bool). Per the memory note, `chat_unread` flips happen inside datastar expressions, not via hidden-input bool round-trips ([[feedback_datastar_underscore_signals]] — UI-only state like the switcher's open menu uses `_`-prefixed signals).
- The composer's existing `body` / `reply_to_id` / `attachment_ids` signals are unchanged; the send handler reads `active_channel` from the bag to route the message.

## Verification

- **Migration backfill:** open a pre-00032 DB with existing chat, migrate, assert every community has exactly one `is_default` `general` channel and every `chat_messages` / `chat_reads` row has a non-null `channel_id` pointing at it. (`internal/chat` service test against `t.TempDir()` per §11.)
- **Default channel guard:** `Service.Delete`/`Archive` on the default channel returns a sentinel error; handler returns 403; UI never renders the controls.
- **Cap:** creating the 11th channel is rejected server-side with a typed error even if the UI is bypassed.
- **Isolation:** two SSE streams on channels A and B; a send to A fat-morphs A's `#messages` only; B's stream sets the unread dot and leaves `#messages` untouched. (Extend `handler_test.go`.)
- **Permissions:** member POST to channel-create returns 403; member can still send to any non-archived channel.
- **Deep-link / redirect:** `/c/{slug}/chat` 302s to `/c/{slug}/chat/general`; an unknown channel slug 404s; an archived channel is read-only (composer hidden, history shown).
- **Smoke:** `make gen && make build && make test`; manual HTTP smoke on a fresh high port (§13) — create a channel, post in it from one tab, watch the unread dot appear in a second tab on #general, click it, see the message.

## Friction

- **Per-channel read receipts** add a column and make `chat_reads` keys `(community_id?, channel_id, user_id)` — settle the exact key + the cross-community/cross-channel uniqueness before writing the migration; getting it wrong means double-counted unread.
- **Unread dot accuracy** on the wire is boolean-cheap (event → dot), but the *initial* dot state on page load needs `UnreadChannels`; a viewer who never opened a channel will show a dot for all history. Decide whether "joined-after" or "all-time" is the unread baseline.
- **Soft cap is a policy, not a law** — ~10 is a UI/clarity choice, not a scaling limit. Document it where admins create channels so it doesn't read as a bug.
- **Channel archive vs message retention** — archived channel history stays queryable; the uploads orphan sweep (§6.7) must treat archived-channel attachments as still-referenced or it'll GC them after 24h.

## Interactions

- Extends [[spec - forumchat - community web app with realtime chat and forum threads]] (the chat surface this scopes).
- Carries over [[spec - chat-attachments - drag-anywhere-multi-mime-extract-to-project]] per channel unchanged.
- Deliberately **decoupled** from [[spec - projects - per-community-collaborative-projects]] / [[spec - project-discussions - per-project-discussion-threads]] — channels are freeform, not per-project (chosen over auto-channel-per-project to avoid lifecycle coupling).
- Distinct from `internal/rooms` (WebRTC video rooms) and `internal/forum` — same word "room/channel", different surfaces; the spec name says "text channels" to avoid confusion.

## Mapping

> [[internal/chat/chat.go]]
> [[internal/chat/handler.go]]
> [[internal/chat/bus.go]]
> [[web/templ/chat.templ]]
> [[web/templ/layout.templ]]
> [[internal/storage/sqlite/migrations/00032_chat_channels.sql]]
> [[cmd/cli/main.go]]

## Future

- {[?]} Private channels — add a `chat_channel_members` table and a join/invite flow; only worth it past ~15-20 members where not-everyone-sees-everything matters.
- {[?]} Optional channel↔project binding — admin links a channel to a project for a jump-to-discussions affordance, without auto-creation.
- {[!]} Per-channel notification preference (mute / all / mentions) once the cross-page notification work (chat_reads, push) is channel-aware.
- {[?]} JetStream replay per channel so a reconnecting client backfills the channel it left off on.

## Notes

- Bootstrap: the migration seeds `general`; the CLI (`cmd/cli`) could grow a `channel add/archive` command mirroring the ban/role pattern, but the admin UI is the primary path — CLI is optional.
- `general` slug is reserved; reject it for new channels to avoid colliding with the default.
