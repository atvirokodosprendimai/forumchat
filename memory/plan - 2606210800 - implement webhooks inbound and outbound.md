---
name: plan-webhooks
status: in-progress
type: plan
spec: spec - webhooks - inbound-bot-messages-and-outbound-event-relay
tldr: Implement per-community webhooks — inbound bot messages (generic + github adapters) and outbound chat relay (slack/discord/generic) — behind WEBHOOKS_ENABLED, mirroring the mailbox feature's shape.
---

# Plan — webhooks (inbound + outbound)

Source of truth: [[spec - webhooks - inbound-bot-messages-and-outbound-event-relay]].
Structural twin: `internal/mailbox` (external ingest, flag, admin CRUD, read-model reuse).

## Phase 1 — schema + bot identity in chat  `[completed when build+test green]`

- `migrations/00042_webhooks.sql`: `webhooks` table (spec Design) + 2 cols on
  `chat_messages` (`bot_name`, `bot_avatar_url`).
- `internal/chat/chat.go`:
  - `KindWebhook Kind = "webhook"`.
  - `Message.BotName`, `Message.BotAvatar`.
  - INSERT column list (`Insert`, `InsertWithAttachments`) carry the 2 cols.
  - `Recent`, `listBefore`, `ByID` SELECT + scan carry the 2 cols; `kind='webhook'`
    populates `AuthorName`/`AuthorAvatar` from bot fields.
  - `Service.PostBot(ctx, cid, channelID, botName, botAvatar, bodyMD) (Message, error)`.
- `web/templ/chat.templ`: `MsgKindWebhook`, `MsgView.IsBot`; bubble suppresses
  author affordances (PM/profile/mention) when `IsBot`.
- Verify: `make gen && make build && make test` (existing chat tests exercise scans).

## Phase 2 — inbound  `[/hooks/{token}]`

- `internal/webhooks/repo.go`: `InboundByToken`, `ListForCommunity`, `Create`,
  `SetEnabled`, `RotateToken`, `Delete`, `Stamp`.
- `internal/webhooks/service.go`: mint token (crypto/rand base64url), validate
  provider×direction.
- `internal/webhooks/adapters.go`: `Adapter` iface, `generic` + `github`,
  `adapterFor`. Pure funcs.
- `internal/webhooks/handler.go`: `PostInbound` (token→row→verify sig→adapter→
  `Chat.PostBot`→fan-out→stamp→200; 404 miss, 401 bad sig, 200 skip).
- `config.go`: `WebhooksEnabled`, `WebhooksMaxBytes`.
- `main.go`: build handler when flag on; mount `r.Route("/hooks", …)` OUTSIDE
  auth, behind httprate + body cap; set `webtempl.WebhooksEnabled`.
- `internal/webhooks/adapters_test.go`: fixtures for generic + github push/ping/bad-sig.

## Phase 3 — admin CRUD + outbound relay

- `web/templ/webhooks.templ`: `/c/{slug}/admin/webhooks` page — inbound + outbound
  sections, create forms, reveal-URL-once, rotate/disable/delete, health column.
- `handler.go`: `GetAdmin`, `PostCreate`, `PostRotate`, `PostToggle`, `PostDelete`.
- mount under the `RequireRole(Admin)` group next to mail-filters; link from `/admin`.
- `internal/webhooks/relay.go`: subscriber on `ChatNewSubject`; load changed
  channel's latest `kind='user'` message; for each matching `direction='out'`
  webhook POST provider payload; skip `kind='webhook'` (no echo); stamp `last_status`.
- `main.go`: spawn relay goroutine when flag on.
- README env-var table + AGENTS.md note.

## Decisions (resolved with user 2026-06-21)

- Direction: **both** in + out.
- Parsing: **generic + github** adapters.
- Matrix: **generic source** (hookshot/maubot); native bridge deferred.
- Identity: **bot-style per webhook** (denormalised name/avatar on chat_messages).

## Progress log

- 2606210800 — spec + plan written; branch `task/webhooks`. Starting Phase 1.
- 2606210815 — **Phase 1 done**: migration 00042 (webhooks table + chat_messages
  bot_name/bot_avatar_url), `chat.KindWebhook` + `Message.BotName/BotAvatar`,
  INSERT + Recent/listBefore/ByID scan sites carry bot cols, `chat.Service.PostBot`,
  `MsgKindWebhook` bubble branch (avatar+name+"bot" tag, mod-delete only, no
  PM/promote), bot CSS. Build + chat tests + full suite green.
- 2606210830 — **Phase 2 done**: `internal/webhooks` repo/service/adapters/handler;
  `generic` + `github` adapters (push/PR/issues/release/ping) + HMAC verify; public
  `POST /hooks/{token}` mounted outside auth behind httprate; `WEBHOOKS_ENABLED` /
  `WEBHOOKS_MAX_BYTES` config. Adapter + signature unit tests + inbound-vertical
  integration test green. Boot smoke: /healthz 200, /hooks/nope 404 (anti-enum).
- 2606210845 — **Phase 3 done**: admin CRUD page `web/templ/webhooks.templ`
  (`/c/{slug}/admin/webhooks`, single create form, reveal-URL-once, toggle/rotate/
  delete, health column) + Admin-index link; outbound `relay.go` wired via
  `chat.Handler.RelayOut` callback (human messages only → no echo loop); slack/
  discord/generic encoders. README env table updated. Full build + `go test ./...`
  green. **Feature complete; awaiting user to commit/merge.**
