---
name: plan-connectors
status: active
type: plan
spec: spec - connectors - external-chat-bots-as-human-members-over-signed-sse
tldr: Build per-community external-chat-bot connectors — a signed long-lived JSON SSE stream + a body-HMAC-signed send endpoint, each backed by a real synthetic member so it acts as a human. Behind CONNECTORS_ENABLED, mirroring the webhooks feature's shape.
---

# Plan — connectors (external chat bots as human members)

Source of truth: [[spec - connectors - external-chat-bots-as-human-members-over-signed-sse]].
Structural twin: `internal/webhooks` (external integration, flag, admin CRUD,
signed bearer, NATS fan-out) — this is the persistent-stream, human-identity,
bidirectional variant.

## Context

Decisions locked with the user (2026-06-25):
- **Identity**: real synthetic member (own `users` + `memberships` row; nick =
  display name) → roster, @mention, profile, reply, mod-delete for free.
- **Signing**: signed URL for the read stream (`uploads.SignShared` shape) +
  body HMAC `X-Signature` for send (`webhooks.verifyGitHubSignature` shape).
- **Wire**: raw JSON `text/event-stream` (machine consumer), NOT datastar.
- **Scope**: many-channel allowlist (`connector_channels`); empty = all.

Grounding (file:line):
- Sibling handler/repo/relay: `internal/webhooks/handler.go:77`,
  `internal/webhooks/repo.go`, `internal/webhooks/relay.go`.
- Chat stream read-loop shape: `internal/chat/handler.go:1573` (`GetStream`).
- Chat send (post as member): `internal/chat/chat.go:1119` (`Service.Send`).
- Synthetic member provisioning: `internal/auth/service.go:396`
  (`activateAndJoin`), `:619` (`UpsertOAuthUser`).
- HMAC primitives: `internal/uploads/uploads.go:365` (`SignShared`/`Verify`),
  `internal/webhooks/adapters.go:206` (`verifyGitHubSignature`),
  `internal/sendtoken/sendtoken.go` (windowed HMAC pattern).
- Fan-out: `internal/webhooks/handler.go:362` (`fanout`),
  `internal/natsx/natsx.go:33` (`ChatSubject`/`ChatNewSubject`).
- Mount precedent: `cmd/app/main.go:1587` (`/hooks/{token}` group, outside auth),
  `:1278` (webhooks wiring), config `internal/config/config.go:196`.

## Phase 1 — schema + identity + signing primitives  `[status: completed when build+test green]`

1. [x] `migrations/00073_connectors.sql`: `connectors` + `connector_channels`
   tables (spec Design). Up + Down.
2. [x] `internal/connectors/connectors.go`: package doc, `Connector` struct,
   `Repo` (ByID, ListForCommunity, Create, SetEnabled, RotateSecret, SetChannels,
   Channels, Delete, Stamp), `ErrNotFound`, scan helper.
3. [x] `internal/connectors/sign.go`: `StreamSig(secret,id,exp)` +
   `VerifyStream` + `VerifyBody(secret, body, header)`. Pure.
   - => unit test `sign_test.go`: round-trip, tamper, expiry, constant-time.
4. [x] `internal/connectors/event.go`: `Mentions(body, nick) bool` +
   message→wire-JSON encoder. Pure.
   - => unit test `event_test.go`.
5. [x] `internal/auth/service.go`: `CreateServiceAccount(ctx, communityID,
   displayName, avatar) (userID, error)` + `RenameMember` + `RemoveServiceAccount`
   (sentinel hash, active, approved membership). Mirrors `activateAndJoin`.
6. [x] `internal/connectors/service.go`: `MemberFactory` interface (consumer
   seam), `Service.Create/Rotate/SetChannels/Rename/Delete`, secret mint
   (crypto/rand), typed errors.
   - => service test against `t.TempDir()` DB (§11): create provisions member +
     membership + channels + 32-byte secret; rotate changes it.

## Phase 2 — public stream + send  `[status: completed when build+test green]`

7. [x] `internal/chat/chat.go` (repo): `ListAfter(ctx, channelID, after, limit)`
   — messages strictly newer than `after`, chronological, with author identity +
   eager attachments, deleted filtered. The stream watermark read model.
8. [x] `internal/connectors/stream.go`: `GetStream` — verify signed URL, raw
   JSON SSE, `event: ready` handshake, Bus + NATS subscribe, per-channel
   watermark, per-message filter (own/deleted/system/mentions_only), heartbeat,
   optional presence bump.
9. [x] `internal/connectors/handler.go`: `PostSend` — `ByID`, body cap, verify
   `X-Signature`, parse JSON, resolve+allowlist channel, `chat.Service.Send` as
   the member, fanout, stamp, `200 {"id"}`.
   - => `handler_test.go`: unknown id 404, bad sig 401, send posts as member,
     foreign channel 403; stream emits one event, skips own, honours mentions_only.

## Phase 3 — admin UI + wiring + flag  `[status: completed when build+test green]`

10. [x] `web/templ/connectors.templ`: `ConnectorsPage` / `ConnectorsContent`
    (stable `#connectors-root`), create form (name/avatar/channel checkboxes CSV
    §6.7/mentions-only), per-row toggle/rotate/edit/delete, reveal-once secret +
    stream URL + send URL + copy snippet. uxui/datastar/frontend-design quality:
    empty/loading/disabled/error states, mobile, a11y, focus states.
    - => `make gen`.
11. [x] `internal/config/config.go`: `ConnectorsEnabled` + `ConnectorsMaxBytes`.
12. [x] `cmd/app/main.go`: wire `connectors.Handler` (MemberFactory = auth.Service,
    chat svc/repo/bus, NATS, ResolveAttachments, BaseURL, presence bump),
    mount `/bots/{id}/stream` + `/bots/{id}/send` (outside auth, httprate),
    admin routes under the community group, `/admin` index link,
    `webtempl.ConnectorsEnabled`.
13. [x] Admin handlers in `handler.go`: GetAdmin, PostCreate (reveal once),
    PostToggle, PostRotate, PostUpdate (channels/mentions), PostDelete.

## Phase 4 — verify + harden  `[status: completed when smoke green + Codex folded]`

14. [x] `make gen && make build && make test` green.
15. [x] Codex read-only review of the diff (untrusted external input: stream +
    send parsers, HMAC verify). Fold confirmed findings.
16. [x] Manual HTTP smoke on a fresh high port (§13): create connector, `curl -N`
    stream, post from a browser tab → JSON event; signed `curl` send → human
    bubble live in the browser. Playwright screenshot of the admin page.

## Verification

See spec Verification. Done = all phases `[x]`, `make test` green, Codex findings
folded, smoke + screenshot captured, plan + memory updated.

## Progress Log

- 2606251323 — Plan created from spec; branch `task/external-chat-bot-connectors`
  off main (latest migration 00072 → new 00073). Four design decisions locked
  with the user.
- 2606251323 — Mid-flight scope add (user): connectors carry an admin-granted
  **capability set** (send/delete/ban/rename) → migration `capabilities` CSV
  column, `Connector.Can`, signed action endpoints + moderation seams.
- 2606251323 — Phases 1-3 shipped (3 commits): schema+repo+sign+identity;
  signed JSON stream + send + actions + `chat.Repo.ListAfter`; admin UI + main.go
  wiring + `CONNECTORS_ENABLED`. `make gen/build/test` green throughout.
- 2606251323 — Phase 4: Codex read-only review (1 high + 4 medium) folded —
  delete-allowlist enforcement, reply_to same-channel validation, archived-channel
  rejection, all-invalid-channel-set refused, stream burst drain loop. Regression
  tests added. Live e2e smoke against the real binary (connector A streams,
  connector B sends a signed message → A receives it, bad-sig 401, anti-enum 404).
  Playwright screenshots of the admin UI; fixed checkbox-picker layout + health dot.
  All phases complete; spec → implemented.
