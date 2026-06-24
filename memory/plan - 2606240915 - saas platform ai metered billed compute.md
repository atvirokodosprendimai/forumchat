---
tldr: Bring platform-provided AI compute back as an authorized, metered, billed per-community opt-in (RAG embed + translate + agents). Super-admin grants free OR Stripe subscription; owner requests → super-admin approves; every platform-compute request metered by token count (in/out) in an append-only ai_usage_events ledger via a decorator installed only on the platform branch of the resolver. Reverses the 2026-06-23 "platform = storage not compute" decision behind metering+billing.
status: completed
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

### Phase 2b - main.go / runner / worker live wiring - status: done

The risky part — installs platform-wrapped vs BYO-bare clients on the real
request paths. Kept separate from 2a so the pure resolver lands verified first.

1. [x] RAG worker (`EmbedderFor` closure): bare embedder cached by (host|model|dim); wrapped per-community with `rag.NewMeteredEmbedder` when `ResolveRAG(...).Platform` (wrap AFTER cache, since the cache is shared across platform communities)
2. [x] Translate closure: when `ResolveTranslate(...).Platform`, `agent.MeteredTranslate` attributing to the requesting member (`auth.FromContext`); else bare `agent.Translate`
3. [x] **Three** agent generation paths via ONE shared `agent.ComputeResolver` seam (commit 2): `agent.Runner` (pane), `agent.Service.SummarizeToThread` (/summary), `chatagents.ThreadRunner` (forum bots). The closure returns the metered platform provider + the model-overridden agent, or the bare BYO provider. Returned agent drives the gen (model comes from `Agent.Model`)
   - => user req: platform offers a TEXT model + a VISION model; `ResolveAgent(s, cfg, vision)` picks by capability; vision agent with no platform vision model stays BYO (never images→text model)
   - => user req: `/summary` summarizer routes to the vision model — `wantsVision = a.Vision || a.IsSummarizer` in the main.go closure (channel summaries include images)
   - => `PLATFORM_AI_AGENT_VISION_MODEL` env added
4. [x] `aiusage.New(db, log)` recorder built in main.go (near debugRec); threaded to the embedder/translate/agent closures
5. [x] Tests: `resolveProvider` seam (override vs BYO), resolver matrix incl. vision; `go test ./...` green. (Full live smoke against a real Ollama deferred to Phase 5.)
   - => agent metering attributes to community (UserID="") for the 3 detached gen paths; translate is per-user. Per-user agent attribution noted as a Phase 5 polish (would thread userID through `Runner.Start`)
   - => committed across 2 commits (2b-i RAG+translate, 2b-ii agent seam + vision)

### Phase 3 - Request → approve lifecycle (no Stripe yet) - status: done

1. [x] Owner `POST /c/{slug}/settings/platform-ai/request` + `/cancel` → `community.RequestPlatformAI`/`CancelPlatformAIRequest`; owner-gated routes
2. [x] Super-admin `/superadmin` Platform AI card: every engaged community + **Grant free** (`GrantPlatformAI`) / **Revoke grant** (`RevokePlatformAI`), audit-logged
3. [x] Owner `/c/{slug}/settings` Platform AI card (stable id `#owner-platform-ai`, outside the form save-morph): request/active/awaiting states + own 30-day usage table
4. [x] Super-admin per-community cost table: rolling 30-day tokens (in/out) + request count from `aiusage.CommunityTotals`, shared-id morph `#sa-platform-ai`
5. [x] State-machine tests (Phase 3a): grant→authorized→platform, revoke→BYO, revoke-keeps-subscription, queue listing
   - => decision: chose direct `community_settings` columns over reusing the `community_requests` table — platform-AI standing is per-community state (one row, mutable), not an append-only request log; `ListPlatformAIRequests` is the queue view
   - => `templ generate` for both `superadmin.templ` + `owner_settings.templ`; `go build ./...` + `go test ./...` green
   - => committed (3a state machine, 3b super-admin card, 3b owner card) + pushed

### Phase 4 - Stripe billing - status: done

1. [x] `internal/billing` leaf pkg over `stripe-go/v82`; env `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_PLATFORM_AI_PRICE_ID` (all three required → `Enabled()`)
2. [x] `POST /c/{slug}/settings/billing/checkout` (owner, `BillingCheckout` seam in admin) → Checkout Session (subscription mode, `client_reference_id`=community) → `sse.Redirect` to Stripe
3. [x] `POST /billing/webhook` (public, **untrusted**) — HMAC signature verify (`ConstructEventWithOptions`, ignore API-version), idempotent by event id, updates subscription status on `checkout.session.completed` + `customer.subscription.updated/deleted`
4. [p] Super-admin "Approve (paid)" intermediate `approved_unpaid` state — deferred: the owner Subscribe button drives checkout directly; super-admin grant is the free path. Not needed for the paid flow to work
5. [x] Owner "Subscribe & activate" button (shown when awaiting + `BillingEnabled`); canceled/past_due/unpaid → `SubscriptionGrantsAccess` false → resolver reverts to BYO automatically
6. [x] **Security gate**: Codex `codex-rescue` read-only review of the webhook diff. Folded confirmed findings:
   - => HIGH (replay/out-of-order): migration **00061** `stripe_events` dedup table (`MarkStripeEventProcessed`, INSERT-OR-IGNORE gate) + stale-subscription guard in `SetSubscriptionStatus` (ignore a lifecycle event for a non-current subscription id) — stops a replayed checkout re-activating a canceled sub, and a late old-sub `deleted` deactivating a live one
   - => HIGH (lost events): customer lookup distinguishes `sql.ErrNoRows` (ignore→200) from transient errors (→5xx so Stripe retries); dedup write error →5xx
   - => MEDIUM (status): `community.SubscriptionGrantsAccess` allowlist (`active`+`trialing`), not a hardcoded `=="active"`
   - => INFO: `http.MaxBytesReader` (clean reject); partial UNIQUE index on `stripe_customer_id` (deterministic customer→community)
   - => tests: forged/missing signature rejected, checkout links + **replay no-ops**, subscription cancel applied, stale-sub guard, status allowlist
   - => recommend user run `/codex:review` or `/codex:adversarial-review` on the branch before relying on live payments
7. [x] `ai_usage_events` already excluded from export (the dataexport manifest is an explicit table allowlist; the ledger was never added) — verified, no change needed
   - => `go build ./...` + `go test ./...` green; committed + pushed

### Phase 5 - Polish + guards - status: done (docs); 2 items deferred

1. [p] Per-community monthly **soft cap** (warn + optional suspend) reading the ledger — deferred (spec `## Future`; the ledger + `agentlimit` precedent are ready). Not needed for the feature to work.
2. [p] BYO↔platform model-swap **reindex** on the switch (different embed dim) — deferred. Today only `admin.PostSettings` auto-reindexes on a RAG change; the grant/request flow does not, so after a switch the platform Qdrant collection is empty until next content write or a manual `/admin` reindex. Documented as a known gap in AGENTS.md §5i. Low-risk (no data loss, converges).
3. [x] Docs: README env tables (`PLATFORM_AI_*` incl. text/vision, `STRIPE_*`) + AGENTS.md (`CLAUDE.md` symlink) **§5i** full feature section
4. [p] Full live smoke (request → grant → agent prompt → ledger row → both panels) — deferred: needs a real Ollama + a Stripe price id. Build + `go test ./...` green is the verification to date; webhook unit-tested + Codex-reviewed.
   - => committed + pushed

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
- 2606241600 — 3rd Codex pass (`codex:rescue` scoped v1.1.3...main). Fixed 3 valid findings: (1) early subscription lifecycle event lost when the customer isn't linked yet → now stamp `community_id` in `SubscriptionData.Metadata` at checkout and resolve lifecycle events from `sub.Metadata` first (customer lookup is the fallback) — kills the event-ordering dependency; (2) bare `StripeSubscriptionStatus == "active"` in 5 places dropped `trialing` subscribers (esp. `RevokePlatformAI` cutting off a trialing customer) → all now use `community.SubscriptionGrantsAccess`; (3) `$sa_pai_cid` not in `InitialSignals` → declared in layout.templ. Tests added: metadata-resolution (early event, trialing), trialing-keeps-on-revoke. `go test ./...` green. Branch `task/saas-platform-ai-rescue-fixes`.
- 2606241500 — Billing hardening (2nd Codex adversarial pass, post-completion). Confirmed all 4 pass-1 fixes hold + no auth bypass. Fixed 3 new findings: HIGH lost-event (dedup claimed the event id BEFORE handle → a failed handle was skipped on retry; now **claim-before-handle + UnmarkStripeEvent release-on-failure**); MEDIUM checkout-trust (was hardcoding `"active"`; now grants only on `payment_status=="paid"`, links ids otherwise, + handle `customer.subscription.created`); MEDIUM concurrency lost-update (`LinkStripeCheckout`/`SetSubscriptionStatus` are now single atomic `UPDATE`s with the stale guard in the `WHERE`, not load-modify-save). New tests: unpaid-checkout-no-grant, handle-failure-releases-claim. `go test ./...` green. Branch `task/saas-platform-ai-billing-hardening`.
- 2606241400 — Phase 4 done. `internal/billing` (Stripe v82): owner checkout + signature-verified public webhook as the sole authority on subscription state. Codex review caught real payment-webhook bugs — folded in: event-id dedup (migration 00061), stale-subscription guard, 5xx-on-transient so Stripe retries, status allowlist (active+trialing), MaxBytesReader, unique customer index. Owner Subscribe button + Stripe env. `go test ./...` green. Branch `task/saas-platform-ai-phase4-stripe`. Verified `ai_usage_events` not in the export allowlist. NEXT: Phase 5 polish (soft cap, docs, live smoke) — and the user should run `/codex:review` before live payments + provide real Stripe price id to test end-to-end.
- 2606241300 — Phase 3 done (3 commits). State machine (`RequestPlatformAI`/`Grant`/`Revoke`/`Cancel`/`ListPlatformAIRequests`) + super-admin grant/revoke + cost table card (`#sa-platform-ai`) + owner request/usage card (`#owner-platform-ai`). Direct `community_settings` columns (not the append-only `community_requests` table) since platform-AI standing is mutable per-community state. `templ generate` both files; build + suite green. NEXT: Phase 4 (Stripe — untrusted webhook, Codex gate, needs the user's Stripe price id), Phase 5 (polish: soft cap, docs, live smoke).
- 2606241200 — Phase 2b done (2 commits). 2b-i: RAG embed + translate metering wired on the existing per-community closures. 2b-ii: shared `agent.ComputeResolver` seam threaded into all THREE agent gen paths (pane Runner, /summary Service, forum ThreadRunner), wired once in main.go. User-driven design additions this session: platform offers TEXT + VISION agent models (`ResolveAgent(s,cfg,vision)` picks by capability; vision-agent-without-vision-model stays BYO), and the `/summary` summarizer routes to the vision model (`wantsVision = a.Vision || a.IsSummarizer`) since channel summaries include images. `go test ./...` green; vet clean. Branch `task/saas-platform-ai-phase2b-wiring`. NEXT: Phase 3 (request→approve lifecycle + owner/super-admin usage UI), then Phase 4 (Stripe).
