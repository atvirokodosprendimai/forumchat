---
tldr: Bring platform-provided AI compute back as an authorized, metered, billed per-community opt-in (RAG embed + translate + agents). Super-admin grants free OR Stripe subscription; owner requests → super-admin approves; every platform-compute request metered by token count (in/out) in an append-only ai_usage_events ledger via a decorator installed only on the platform branch of the resolver. Reverses the 2026-06-23 "platform = storage not compute" decision behind metering+billing.
status: open
---

# Plan: SaaS platform AI — metered, billed per-community compute

## Context

- Spec: [[spec - saas-platform-ai - operator-provided metered billed ai compute per community]]
- Extends: [[spec - saas-tenant-config - per-community owner-configurable ai rag translate storage and join policy]] (its `## Future` "quotas/billing per tenant")
- Reverses (behind guards): diary `SESSION7b:2026-06-23` "platform provides STORAGE+MESSAGING, NOT compute"
- Reused precedents:
  - resolver: `internal/community/resolve.go` (`ResolveRAG:62`, `ResolveTranslate:85`, `EffectiveAIEnabled:49`)
  - settings table + sealing: `internal/community/settings.go` (migration 00055)
  - request→approve queue: `internal/community/requests.go` (migration 00056)
  - per-community caps shape: `internal/agentlimit/agentlimit.go`
  - nil-safe async recorder shape: `internal/debuglog` (§5d)
  - super-admin shared-id morph: `internal/superadmin/handler.go` (`SADebugCard`)
- Latest migration: `00058` → new migrations are `00059`, `00060` (confirm at write).

## Decisions carried in (user, 2026-06-24)

- Billing: super-admin can **grant free**; else **active Stripe subscription** required.
- Metering grain: **token counts in/out**, by feature + community (+ user when human-triggered); meter **only** platform-compute requests.
- Toggle authority: owner **requests** → super-admin **approves** (free or paid).
- This turn produced **spec + plan only** — no code yet. Phases below are the build order for when implementation is greenlit.

## Phases

### Phase 0 - Metering bedrock (ledger + recorder + token surfacing) - status: done

The foundation everything else needs. No billing, no toggle yet — just "can we
count platform-compute tokens and store them."

1. [x] Migration `00059_ai_usage_events.sql` — append-only ledger
   - cols per spec: id, community_id (FK cascade), feature, user_id (FK SET NULL), model, tokens_in, tokens_out, estimated, created_at
   - index `(community_id, created_at)` for date-range rollups
2. [x] New leaf package `internal/aiusage` — `Event` struct + nil-safe concrete `Recorder` (mirrors `debuglog.Recorder` exactly: concrete `*Recorder`, nil-safe, log-not-return on write failure)
   - => chose concrete nil-safe `*Recorder` over an interface — matches the established `debuglog` precedent the spec cites; decorators (Phase 1) hold the concrete type
   - => imports only `database/sql` + `uuid` (true leaf); read-side `Rollup(communityID, from, to) → []FeatureTotal` + `CommunityTotals(from, to) → []CommunityTotal`
   - => `Record` synchronous (not async) — small insert at end of a generation that already writes every 100ms; deterministic to test. Async buffering noted as a possible later optimization, not needed now
3. [x] Surface token usage from the provider
   - `Usage{PromptTokens, CompletionTokens int}` added to `StreamResult` (`internal/agent/provider.go`)
   - Ollama `done` object `prompt_eval_count` + `eval_count` parsed into `ollamaChatChunk` → `res.Usage` on the done chunk
   - => `Generate` NOT changed to sum/return usage — Phase-1 decorator wraps `Provider.Stream`, recording one ledger row per provider turn; the agentic loop's turns sum naturally via multiple rows + rollup. Simpler than threading a return value through the shared core
4. [x] Unit tests: ledger insert/rollup/totals round-trip + nil-safe + dropped-when-missing-dims; Ollama usage parse (httptest NDJSON)
   - => `go build ./...` ok, `go test ./...` green
   - => committed + pushed

### Phase 1 - Metering decorators (meter iff platform) - status: done

1. [x] `agent.meteredProvider` wrapping `Provider` — records one row per turn on `Stream` (community/user/agent/model + real usage); `internal/agent/metering.go`
2. [x] `rag.meteredEmbedder` wrapping `Embedder` — records on Embed, tokens estimated from input len, `Estimated=true`; `internal/rag/metering.go`
3. [x] `agent.MeteredTranslate` — wraps the package-level `Translate` (not an interface), records estimated in/out tokens; same file
   - => `Translate` is a func, not an interface, so a metered *function* wrapper (not a type) is the right shape; the platform-vs-BYO choice (Phase 2) calls `MeteredTranslate` vs `Translate`
4. [x] Each decorator implements the SAME interface it wraps; `New*` returns the inner client unwrapped when `rec==nil || communityID==""` so the BYO path pays nothing
   - => shared token estimate lives in `aiusage.EstimateTokens` (utf8 runes/4) — DRY across both decorators rather than duplicated per package
5. [x] Tests: wrapped client records exactly one correctly-dimensioned row; bare/nil-rec client records zero (`agent` + `rag` external test packages, real recorder + temp DB + seeded community/user FK)
   - => agent test caught the `user_id` FK: a fake user id fails the insert (silently, since Record swallows errors) — must seed a real users row; documented in the test
   - => `go test ./...` green; committed + pushed

### Phase 2a - Platform config + resolver tier (pure, testable) - status: done

1. [x] `PLATFORM_AI_*` env in `internal/config/config.go` (RAG baseurl/model/dim + qdrant url/key; translate baseurl/model; agent provider/baseurl/model/key) — separate namespace from BYO, all default empty → inert
2. [x] Migration `00060_platform_ai_settings.sql` — `community_settings` cols: use_platform_ai, platform_ai_status, platform_ai_granted_free, stripe_customer_id/subscription_id/subscription_status, platform_ai_requested_at (all NULL)
3. [x] Extended `community.Settings` struct + `Settings`/`SaveSettings` scan/upsert for the new cols (non-secret, no sealing)
4. [x] `community.PlatformAI(s, cfg) (on, authorized)` + platform tier in `ResolveRAG`/`ResolveTranslate` + new `ResolveAgent`; `Platform bool` marker on each Effective* struct
   - => `usePlatform()` guard requires the relevant `PLATFORM_AI_*` endpoint configured, else falls through to BYO — operator must explicitly "open for business" per capability
   - => platform branch defaults the per-feature enable ON (`boolOr(s.RAGEnabled, true)`) vs BYO default OFF; kill-switch (`cfg.RAGEnabled`) still gates both; per-community Qdrant collection name preserved on platform for isolation
5. [x] Table tests: authorization matrix (grant / sub-active / canceled / unauthorized), platform-tier RAG+translate+agent, unset-endpoint fallthrough, kill-switch over platform; `SAAS=false` unchanged (existing tests green)
   - => `go test ./...` green; committed + pushed

### Phase 2b - main.go / runner / worker live wiring - status: open

The risky part — installs platform-wrapped vs BYO-bare clients on the real
request paths. Kept separate from 2a so the pure resolver lands verified first.

1. [ ] RAG worker: per-community embedder built from `ResolveRAG`; when `.Platform`, wrap with `rag.NewMeteredEmbedder(_, rec, communityID)`
2. [ ] Translate handler: when `ResolveTranslate(...).Platform`, call `agent.MeteredTranslate(_, rec, cid, uid, ...)` else `agent.Translate(...)`
3. [ ] Agent runner / thread runner: when `ResolveAgent(...).Platform`, build provider from platform config + wrap with `agent.NewMeteredProvider(_, rec, cid, uid)`; else the agent's BYO provider (bare)
4. [ ] Build `aiusage.New(db, log)` recorder in main.go; thread it to worker + translate + runners via the existing closure seams
5. [ ] Smoke: opted-in+authorized community → agent/translate/embed each write a ledger row; BYO community writes none

### Phase 3 - Request → approve lifecycle (no Stripe yet) - status: open

1. [ ] Owner `POST` "request platform AI" → status=requested (reuse `community_requests` shape or extend it)
2. [ ] Super-admin `/superadmin` Platform AI section: pending requests, **Grant free** (granted_free=1 → active) / **Deny**
3. [ ] Owner `/c/{slug}/settings` Platform AI card: toggle + status display + usage summary (Phase 0 rollup)
4. [ ] Super-admin usage table: per-community tokens/requests by feature + grand totals (shared-id morph like `SADebugCard`)
5. [ ] Authorization transition tests: grant → authorized → platform path; grant removed → BYO/off path, no new ledger rows
   - => commit + push

### Phase 4 - Stripe billing - status: open

1. [ ] `internal/billing` leaf pkg over `stripe-go`; env `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PLATFORM_AI_PRICE_ID`
2. [ ] `POST /c/{slug}/settings/billing/checkout` (owner) → Stripe Checkout Session → redirect
3. [ ] `POST /billing/webhook` (public, **untrusted**) — verify signature, idempotent by event id, update `stripe_subscription_status` + `platform_ai_status` on `customer.subscription.*`
4. [ ] Super-admin "Approve (paid)" path: status=approved_unpaid → owner checkout → webhook active
5. [ ] Owner "Subscribe" button + lapsed-subscription notice; canceled/past_due → unauthorized → revert to BYO/off
6. [ ] **Security gate**: Codex review (`codex:codex-rescue` read-only) on the webhook diff before merge; recommend `/codex:review` to user. Tests: forged signature rejected, valid event applied, replay idempotent
7. [ ] Exclude `ai_usage_events` from `internal/dataexport` manifest (platform property)
   - => commit + push

### Phase 5 - Polish + guards - status: open

1. [ ] Per-community monthly **soft cap** (warn + optional suspend) reading the ledger (defer hard quota)
2. [ ] BYO↔platform model-swap reindex prompt on the toggle (different embed dim)
3. [ ] Docs: README env table (`PLATFORM_AI_*`, `STRIPE_*`); forumchat CLAUDE.md §5f/§5h sibling section; super-admin CLAUDE.md note
4. [ ] Full smoke (spec Verification): request → grant → agent prompt → ledger row → both panels show it
   - => commit + push

## Verification

See spec `## Verification`. Gate before each commit: `make gen && make build && make test`.
Acceptance: an opted-in+authorized community's agent/RAG/translate requests each
write one correctly-dimensioned `ai_usage_events` row with token counts; a BYO
community writes none; `SAAS=false` mounts nothing and existing tests stay green;
Stripe webhook is the sole authority on subscription state and rejects forged
signatures.

## Adjustments

- 2606240915 — plan created from spec; scope this turn = spec + plan only (user choice). Build order above is for the implementation greenlight.

## Progress Log

- 2606240915 — Bootstrapped session (effective-go + specs + code graph + palace). Surfaced the conflict with the 2026-06-23 BYO-only decision; user confirmed the reversal is intended behind metering+billing. Clarified 4 scoping decisions. Wrote spec `[[spec - saas-platform-ai - ...]]` + this plan. No code.
- 2606241000 — Phase 0 done. Migration 00059 ai_usage_events; `internal/aiusage` (Event + nil-safe Recorder + Rollup/CommunityTotals); `StreamResult.Usage` surfacing Ollama prompt_eval_count/eval_count. Tests green (`go test ./...`). Design note: metering will be per-provider-turn rows in the Phase-1 decorator, so `Generate` stays unchanged. Branch `task/saas-platform-ai-phase0`.
- 2606241030 — Phase 1 done. Metering decorators: `agent.NewMeteredProvider` (real token usage per turn), `rag.NewMeteredEmbedder` + `agent.MeteredTranslate` (estimated via `aiusage.EstimateTokens`). All nil-safe passthrough when unwired. Tests prove meter-iff-platform (wrapped records, bare records zero). `go test ./...` green. Branch `task/saas-platform-ai-phase1`.
- 2606241100 — Phase 2a done. `PLATFORM_AI_*` env (separate namespace) + migration 00060 (community_settings platform cols) + Settings load/save + `PlatformAI()`/`ResolveAgent()` + platform tier in `ResolveRAG`/`ResolveTranslate` with `Platform` markers. Resolver table tests cover the full authorization matrix + fallthrough + kill-switch. `go test ./...` green. Branch `task/saas-platform-ai-phase2a`. Split Phase 2 into 2a (pure/done) + 2b (live main.go/runner/worker wiring — next, riskier).
