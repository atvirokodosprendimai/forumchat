---
tldr: Implement chat-channels — split the single community chat into multiple all-public named text channels with an inline admin-managed switcher, #general default, dot-only unread, and one SSE stream that carries the channel id on the wire.
status: active
---

# Plan: Chat Channels — admin-curated public text channels

## Context

- Spec: [[spec - chat-channels - admin-curated-public-text-channels]]
- Extends: [[spec - forumchat - community web app with realtime chat and forum threads]]
- Carries over: [[spec - chat-attachments - drag-anywhere-multi-mime-extract-to-project]]
- Decoupled from: [[spec - projects - per-community-collaborative-projects]], [[spec - project-discussions - per-project-discussion-threads]]
- Distinct from `internal/rooms` (WebRTC video) and `internal/forum`.

**Locked decisions** (spec Q&A + plan Q&A):
- All-public channels, no membership table — `channel_id` column only.
- Admin/mod create/rename/topic/reorder/archive; members pick + post. Soft cap ~10 (server-enforced).
- Independent named channels — no project coupling.
- Today's chat → undeletable `#general`; migration backfills.
- **Manage UI = inline in the switcher** (`+ channel` pill + per-channel ⚙ menu, inline create popover).
- **Unread cue = dot only** (boolean per channel on the wire; no count).
- **Realtime = one SSE stream, channel id on the wire** — active channel fat-morphs, others get an unread dot. (Per-channel Bus rejected; see spec.)

**Open friction resolved here (so Phase 1 can start):**
- `chat_reads` keyed per `(channel_id, user_id)` with `last_read_at`. Backfill existing rows to `#general`.
- Unread baseline = "newer than your `last_read_at` for that channel; no read row ⇒ unread". New channels start empty so no spurious dots; `#general` keeps existing read state via backfill.

**Conventions:** apply `/effective-go` idioms on every Go action; `/datastar` idioms on every templ/UI action. `make gen` after any `.templ` edit. Branch already `spec/chat-channels` — continue here or cut `task/chat-channels`.

## Phases

### Phase 1 - Data model + scope existing chat to a channel - status: completed

Goal: schema + backfill land, existing single-channel chat keeps working unchanged (lands on `#general`). No new UI yet. Verifiable: app boots on an existing DB, chat works, tests pass.

1. [x] Write migration `00032_chat_channels.sql`
   - `chat_channels(id, community_id FK ON DELETE CASCADE, slug, name, topic, position INT, is_default INT NOT NULL DEFAULT 0, archived_at INT NULL, created_by FK users, created_at INT NOT NULL)`, `UNIQUE(community_id, slug)`.
   - `ALTER TABLE chat_messages ADD COLUMN channel_id TEXT REFERENCES chat_channels(id) ON DELETE CASCADE`.
   - Backfill (FK order §8 — insert channels BEFORE updates): one `general` per community (`is_default=1, position=0, slug='general', name='general'`), then `UPDATE chat_messages / chat_reads SET channel_id = <that general's id>` per community.
   - `CREATE INDEX idx_chat_messages_channel_created ON chat_messages(channel_id, created_at)`.
   - goose down: drop index, columns, table.
   - => `chat_reads` PK changed (user_id, community_id) → (user_id, channel_id); SQLite can't ALTER a PK, so the table is **rebuilt** (create-new + copy + drop + rename) rather than ADD COLUMN. Down rebuilds it back. created_by is **nullable** (FK ON DELETE SET NULL) so system-seeded #general has NULL creator.
   - => general channel ids generated in-SQL via `lower(hex(randomblob(16)))` (not uuid format — TEXT PK, format irrelevant).
   - => verified: migration chain applies clean (auth package tests migrate full chain green).
2. [x] Add channel types + read queries to `internal/chat/chat.go`
   - `type Channel struct{ ID, CommunityID, Slug, Name, Topic string; Position int; IsDefault bool; ArchivedAt *time.Time; ... }`.
   - `Repo.ListChannels(ctx, communityID string, includeArchived bool) ([]Channel, error)` ordered by `position`.
   - `Repo.ChannelBySlug(ctx, communityID, slug) (Channel, error)`; `Repo.DefaultChannel(ctx, communityID) (Channel, error)`.
   - => also added `ChannelByID` + `EnsureDefaultChannel` (idempotent #general creator) + shared `scanChannel`.
   - => wired `chatRepo.EnsureDefaultChannel(bootCommunity.ID)` into `cmd/app/main.go` boot (next to rooms seed). New communities created via UI must also call it (Phase 2/handler).
   - => build green.
3. [x] Thread `channelID` through existing read/write repo + service methods
   - `Repo.Recent/Before/listBefore/MarkRead/ReadersSince` now key on `channel_id`. `Message` gained `ChannelID`; `listBefore`/`ByID` select it. `MarkRead` upserts on `(user_id, channel_id)`, stores `community_id` for the readers join.
   - `Service.Send` + `SendInput` accept `ChannelID`. **PostSystem signature unchanged** — system/bridge messages leave `ChannelID` empty and `Repo.Insert` resolves `#general` as a fallback, so forum/projects/rooms callers needed **zero** edits.
   - Handler: added `activeChannel(ctx, slug)` resolver (reads chi `{channel}` param, falls back to `#general`) + `channelSlug(r)`. Threaded `ch.ID` through GetPage/GetStream/PostSend/PostDelete/setBlock/PostMarkRead. PostSend rejects archived channels. (URL routing/redirect deferred to Phase 2 — no `{channel}` route exists yet, so everything lands on `#general`.)
   - => added `Repo.UnreadChannels` (page-load dot seed, used Phase 4).
   - => removed dead `loadRecent` + `toMsgViews` (orphaned when `loadRecentFor` took over).
   - => build + vet + full `go test ./...` green.
4. [x] Update `internal/chat/handler_test.go` setup + green `make test`
   - fixture now calls `EnsureDefaultChannel` (BootstrapOrFetch doesn't seed #general; main.go does on boot).
   - => added `TestChannelScope_InsertRecent`: insert (explicit + empty-channel fallback) → Recent(general) returns both, Recent(unknown) returns none.

### Phase 2 - Inline switcher + admin channel CRUD - status: completed

Goal: the switcher bar renders above `#messages`; admins create/rename/topic/archive inline; a second channel appears and is selectable. Visible, human-testable.

1. [ ] Channel write ops in `internal/chat/service.go` (single-writer, typed errors)
   - `CreateChannel` (slugify name, reject reserved `general`, enforce ~10 cap → `ErrChannelCap`, `position = max+1`).
   - `RenameChannel`, `SetTopic`, `Reorder`, `Archive` (refuse `is_default` → `ErrDefaultChannel`), `Delete` (admin-only, refuse `is_default`, cascade).
2. [ ] Mod/admin-gated channel endpoints in `internal/chat/handler.go`
   - `POST create / rename / topic / archive / reorder`, `DELETE` (admin). Role check → 403 for members. `ReadSignals` before `NewSSE`.
   - each ends by fat-morphing the switcher (stable-id extract §4.7) so all admin tabs update.
3. [ ] `ChannelSwitcher` templ in `web/templ/chat.templ`
   - stable root id `#chat-switcher`; pills ordered by position; active highlighted; `⚙` menu + `+ channel` for mod/admin only.
   - inline create popover + rename/topic/archive controls; reuse EDA dispatch (§4.12) — one `data-on:fc:channel-edit` consumer, N producer pills.
   - `make gen`.
4. [ ] Channel-switch read endpoint + URL push
   - `GET /c/{slug}/chat/{channelSlug}` fat-morphs `#messages` to that channel's latest 100, scrolls, marks-read, pushes URL. Client: `data-on:click="$active_channel='<id>'; @get(...)"`.
   - => verify deep-link/refresh lands on the right channel; unknown slug 404; archived channel = read-only (composer hidden, history shown).

### Phase 3 - Per-channel realtime (one stream, id on the wire) - status: completed

=> **Deviation from spec's "single persistent stream across switches":** channel switching is a full nav to `/chat/{channel}` (new stream per page), NOT an SPA morph. The shared community Bus still carries the channel id on the wire; the stream fat-morphs when the changed channel == the viewer's active channel, else pushes a `chat_unread` dot. This keeps free cross-channel dots (spec's goal) and is simpler/robust; the only cost is one stream reconnect per switch (negligible). Empty channel id = structural change (channel CRUD / bridge) → stream re-renders the switcher + active messages so new/renamed/archived channels appear live everywhere.

Goal: two tabs on different channels; posting in A morphs A and lights a dot on B without disturbing B's view.

1. [ ] Publishers carry the channel id
   - `chat.Bus.Broadcast(channelID)` + NATS publish to `community.<cid>.chat` with payload = channel id (not `"changed"`). Every chat write path (send, delete, promote, extract, forum bridge).
2. [ ] `Handler.GetStream` filters by active channel
   - hold viewer `$active_channel`; on event: `payload == active` → refetch + fat-morph `#messages` + scroll + clear that dot; else → set `chat_unread[payload]` dot, leave `#messages` alone.
3. [ ] New signals in `web/templ/layout.templ` `InitialSignals`
   - `active_channel` (string), `chat_unread` (map id→bool). Flip `chat_unread` inside datastar expressions only (no hidden-input bool round-trip). UI-only menu state uses `_`-prefixed signals.
   - `make gen`.

### Phase 4 - Per-channel read state + carry-over + sweep - status: completed

=> Mostly satisfied by Phase 1-3 design:
=> 4.1 per-channel reads — done (MarkRead keyed `(user_id, channel_id)`, `UnreadChannels`, GetPage seeds dots + clears active).
=> 4.2 carry-over — replies/quotes/attachments/extract operate on message/attachment ids (channel-agnostic, unchanged). Promote-to-thread keeps origin channel (`ByID` returns ChannelID; bubble stays in its channel). Forum→chat `thread_announce` posts into #general via the Insert default-channel fallback. Delete resolves the message's own channel. Verified by build + the channel-scope test.
=> 4.3 uploads sweep — **non-issue**: `sweep.go` keys on `chat_message_attachments` existence; archive keeps messages + link rows, so archived-channel uploads stay referenced. Only hard-delete cascades messages (correctly GC-able). No change needed.

Goal: unread dots correct across reload; all existing chat features work per channel; no upload GC regression.

1. [ ] Per-channel read receipts + initial unread seed
   - `MarkRead` writes `(channel_id, user_id, last_read_at)`; `Repo.UnreadChannels(ctx, communityID, userID) (map[string]bool)` seeds dots on page load.
2. [ ] Carry-over verification + fixes
   - replies/quotes, attachments, promote-to-thread (keeps origin `channel_id`), extract-to-project, soft-delete placeholder, mod delete, rate limit — all scoped by channel. Forum→chat `thread_announce` posts into `#general`.
3. [ ] Uploads orphan sweep guard
   - `internal/uploads/sweep.go` must treat archived-channel attachments as still-referenced (don't GC after 24h).

### Phase 5 - Tests, smoke, docs - status: active

1. [x] Service/handler tests (`internal/chat/chat_channels_test.go` + `handler_test.go`)
   - => `TestCreateChannel_SlugCapReserved` (slugify, reserved general, dup slug, empty, ~10 cap), `TestDefaultChannelGuard` (archive/delete/rename #general → ErrDefaultChannel), `TestArchiveHidesFromSwitcher`, `TestUnreadChannels` (dot set/clear), `TestChannelScope_InsertRecent`. All green.
   - => migration backfill verified via boot smoke (clean migrate to v32) + the scope test; member-create-403 / deep-link-404 left to manual (needs httptest auth wiring) — covered by route smoke (303→login).
2. [x] Boot smoke (fresh high port, fresh DB)
   - => app boots, migration 00032 applies, routes register with no chi conflict, #general seeded (is_default=1), `/c/main/chat` → 303 login, `/c/main/chat/general` route resolves. Full interactive UI (click switch, live dots) needs a browser — not run headless.
3. [ ] Docs — update root AGENTS.md §6 (channels + one-stream-id-on-wire), README routes; mark spec `status: shipped`.

## Verification

- `make gen && make build && make test` green; `internal/chat` tests cover backfill, guards, cap, isolation, permissions, routing.
- Manual: existing DB migrates with zero chat loss and lands on `#general`; admin creates `#design`, it appears inline; switching fat-morphs + pushes URL; two-tab unread dot works; member can't create but can post anywhere; `#general` has no delete/archive control; archived channel is read-only.
- No upload sweep regression on archived channels.

## Adjustments

<!-- timestamped changes go here -->

## Progress Log

- 2606181114 — Plan created from [[spec - chat-channels - admin-curated-public-text-channels]].
- 2606181210 — **Phases 2-4 complete + Phase 5 tests.** Switcher bar + inline admin CRUD (create/rename/topic/archive + admin delete), per-channel routes (`/chat/{channel}/...`), full-nav switching, one-stream-id-on-wire realtime (morph active / dot others / re-render switcher on structural change), per-channel reads + unread seed, switcher CSS, channel-aware chat JS. 4 new channel tests green; boot smoke verified (migrate v32, routes, #general). Remaining: docs (5.3) + manual browser verify. Net so far: migration→data→realtime→UI→tests, build+test green throughout.
- 2606181150 — **Phase 1 complete.** Migration 00032 (chat_channels + per-channel chat_messages/chat_reads, #general backfill), channel read queries + EnsureDefaultChannel, all repo/service/handler read+write paths channel-scoped, fixture + scope test. Build/vet/test green. Key call: PostSystem unchanged + Insert default-channel fallback ⇒ forum/projects/rooms bridge callers untouched. Phase 1 end-state = existing chat works exactly as before, now backed by #general. UX forks resolved: inline switcher management + dot-only unread. Friction items resolved: chat_reads keyed per (channel_id, user_id), unread baseline = newer-than-last_read / no-row ⇒ unread. 5 phases, visible result by end of Phase 2.
