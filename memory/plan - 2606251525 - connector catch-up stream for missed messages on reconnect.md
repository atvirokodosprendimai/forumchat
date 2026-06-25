---
name: plan-connector-catchup
status: active
type: plan
tldr: Give external connectors a backlog replay so a worker that reconnects after a disconnect receives the messages it missed. A `?since=<unix>` query param on GET /bots/{id}/stream seeds the per-channel watermark from that point (clamped to a max look-back window), drains the backlog once, emits an `event: live` boundary marker, then continues live. SDK gains StreamSince + OnLive; the tinychat example tracks the newest message and reconnects with catch-up; dev docs + SDK comments updated.
---

# Connector catch-up stream — missed-message replay on reconnect

## Context

- Spec: [[spec - connectors - external-chat-bots-as-human-members-over-signed-sse]]
  lists this exact gap under **Friction** and **Future**:
  > "Live-only stream (v1). A reconnecting worker misses messages sent while it
  > was away (watermark resets to connect-time). JetStream-backed replay / a
  > `?since=` backlog is Future."
- Code reality:
  - `internal/connectors/stream.go:91-102` seeds `wm[id] = now` and pre-seeds the
    boundary-second `seen` set → **live-only** by construction.
  - `internal/connectors/stream.go:166` `drainChannel` is already the watermark
    read model: it loops `chat.Repo.ListAfter` until caught up. Catch-up reuses
    it verbatim — only the *initial* watermark changes.
  - `internal/chat/chat.go:693` `listAfter` = `created_at >= after ASC LIMIT` —
    inclusive boundary, second granularity. Seeding `wm[id] = since` and draining
    once delivers the backlog with zero new SQL.
  - SDK `sdk-go/connector.go:132` `StreamURL(exp)` builds the signed URL;
    `Stream(ctx,h,exp)` is one-shot by design (`:146`); `examples/tinychat/main.go:88`
    `streamLoop` already does reconnect+backoff — the natural home for catch-up.
- Past decision: [[project_dev_docs_site]] — docs live in `docs/connectors.md`
  (trusted goldmark), SDK is the source of truth for the wire.

## Decisions

- **`?since=<unix>` (timestamp), not `?last_id=`.** The server's watermark model
  is already timestamp + boundary-second seen-set; a timestamp slots in with no
  new query. The worker persists the newest `created_at` it saw.
- **Bounded by a time clamp** (`maxCatchupWindow`, 24h) — a signed URL is a bearer
  capability; an unbounded `since=0` would replay all history on a public route.
  Clamp `since` to `now - window`; older requests are silently truncated (the
  `live` marker reports it). No per-count cap needed — the clamp + batched drain
  bound memory. `since` is intentionally NOT part of the HMAC: only a secret-holder
  can reach the stream at all, and they only ever read their own channels, so the
  worst a forged `since` does is cost — already clamped.
- **`event: live` boundary marker**, emitted after the initial drain in *both*
  modes, so the contract is uniform: `ready` → [backlog `message`s] → `live` →
  [live `message`s]. Carries `{since, truncated}` so the worker knows the
  effective watermark and whether older messages were dropped. Backward
  compatible — existing workers ignore unknown events.

## Phases

### Phase 1 — server: `?since=` + bounded backlog + `live` marker  [active]
1. [ ] `stream.go`: parse `since`, compute `effectiveSince` (clamp to window,
   ignore future), seed `wm`/`seen` accordingly; live-only path unchanged when
   `since` absent.
2. [ ] Drain all channels once after the handshake (no-op in live-only mode),
   then emit `event: live` `{since, truncated}` before the select loop.
3. [ ] `make build` green.
   - verify: `go build ./...`

### Phase 2 — SDK: StreamSince + OnLive  [open]
4. [ ] `connector.go`: `StreamURL` gains `since`; add `StreamSince(ctx,h,exp,since)`,
   keep `Stream` as the zero-since wrapper (compat); `Live` struct + `OnLive` in
   `Handlers`; dispatch `live`.
5. [ ] `sdk-go` tests + `go test ./...`.
   - verify: `go test ./sdk-go/...`

### Phase 3 — example: tinychat reconnects with catch-up  [open]
6. [ ] `main.go`: track newest `created_at` seen (guarded), pass it as `since` on
   reconnect; print a dim "caught up" line on `OnLive` when backlog was replayed.
   - verify: `go build ./examples/tinychat`

### Phase 4 — docs  [open]
7. [ ] `docs/connectors.md`: replace "live-only" with the catch-up contract;
   `?since=` curl; `event: live`; SDK `StreamSince`/`OnLive`; reconnect section.

### Phase 5 — tests, verify, review  [open]
8. [ ] `internal/connectors` stream test: a message before connect is replayed
   with `since`, not without; `live` marker fires; clamp honored.
9. [ ] Codex read-only review (public untrusted-input surface: the `since` param).
10. [ ] `make gen && make build && make test`; commit per phase, push.

## Verification

- A connector that connects with `?since=<t>` receives messages created after `t`
  that predate the connection; without `since` it does not (live-only preserved).
- `since=0` / a very old `since` is clamped to the window; the `live` marker
  reports `truncated:true`.
- The connector's own messages are still never echoed, even in the backlog.
- SDK round-trips: `StreamSince` builds a `?since=` URL; `OnLive` fires once.
- `make test` green; Codex finds no correctness/security defect in the param path.
