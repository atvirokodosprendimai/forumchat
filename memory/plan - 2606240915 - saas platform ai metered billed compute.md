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

### Phase 0 - Metering bedrock (ledger + recorder + token surfacing) - status: open

The foundation everything else needs. No billing, no toggle yet — just "can we
count platform-compute tokens and store them."

1. [ ] Migration `00059_ai_usage_events.sql` — append-only ledger
   - cols per spec: id, community_id (FK cascade), feature, user_id (FK SET NULL), model, tokens_in, tokens_out, estimated, created_at
   - index `(community_id, created_at)` for date-range rollups
2. [ ] New leaf package `internal/aiusage` — `Event` struct, `Recorder` interface, `SQLRecorder` (async/buffered insert, nil-safe no-op like `debuglog.Recorder`)
   - must NOT import community/rag/agent (leaf; consumers declare the interface)
   - read-side: `Rollup(ctx, communityID, from, to) → []FeatureTotal` + `AllCommunityTotals(ctx, from, to)` for the panels
3. [ ] Surface token usage from the provider
   - add `Usage struct{PromptTokens, CompletionTokens int}` to `StreamResult` (`internal/agent/provider.go:31`)
   - parse Ollama `done` object `prompt_eval_count` + `eval_count` into `ollamaChatChunk` (provider.go:116) → `StreamResult.Usage`
   - `agent.Generate` (`internal/agent/generate.go:27`) sums usage across the agentic loop and returns it
4. [ ] Unit tests: ledger insert/rollup round-trip; Ollama usage parse; Generate sums multi-turn
   - => commit + push

### Phase 1 - Metering decorators (meter iff platform) - status: open

1. [ ] `agent.meteredProvider` wrapping `Provider` — records on `Stream` (community/user/agent/model + usage)
2. [ ] `rag.meteredEmbedder` wrapping `Embedder` — records on Embed (estimated token count from input len, `estimated=1`)
3. [ ] `agent.meteredTranslator` (or wrap the translate client in `internal/agent/translate.go`) — records on Translate (estimated)
4. [ ] Each decorator implements the SAME interface it wraps (accept-interfaces); callers untouched
5. [ ] Tests: wrapped client records exactly one row with right tokens; bare client records zero
   - => commit + push

### Phase 2 - Platform config + resolver tier - status: open

1. [ ] `PLATFORM_AI_*` env in `internal/config/config.go` (RAG baseurl/model/dim/qdrant url+key; translate baseurl/model; agent provider/baseurl/model/key)
   - separate namespace from BYO `RAG_*`/`TRANSLATE_*` — keeps default inert (operator pays zero when unset)
2. [ ] Migration `00060_platform_ai_settings.sql` — `community_settings` cols: use_platform_ai, platform_ai_status, platform_ai_granted_free, stripe_customer_id, stripe_subscription_id, stripe_subscription_status, platform_ai_requested_at (all NULL/default-off)
3. [ ] Extend `community.Settings` struct + `Settings`/`SaveSettings` scan/upsert (settings.go) for the new cols
4. [ ] `community.PlatformAI(s, cfg) (on, authorized bool)` in resolve.go; `ResolveRAG`/`ResolveTranslate` + agent-provider resolution return platform config when `on && authorized`, else fall through to today's BYO/override
5. [ ] `main.go`: build platform clients from `PLATFORM_AI_*`, wrap in metering decorators; resolver closures pick platform-wrapped vs BYO-bare per community
6. [ ] Table tests for the resolver tier (5 cases in spec Verification); confirm `SAAS=false` still returns env (existing tests green)
   - => commit + push

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
