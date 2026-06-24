---
tldr: When SAAS=true a community can opt to run RAG embedding, translation, and agents on the PLATFORM'S OWN AI compute ("system-wide settings") instead of BYO. Access is authorized two ways — a super-admin grants it free, or the owner subscribes via Stripe — and is requested owner→approved super-admin (community_requests pattern). Every request that uses platform compute is metered by token count (in/out) per community/feature/user in an append-only ai_usage_events ledger, so the operator can bill and analyse exactly where their compute was spent. Metering is a decorator over the platform Provider/Embedder/Translator, installed ONLY on the platform-compute branch, so it structurally cannot meter BYO and "everywhere system settings applied" is guaranteed without scattered call-site edits. Reverses the 2026-06-23 "platform provides storage+messaging, NOT compute" decision — but only behind metering+billing, which neutralises the original unbounded-cost concern.
---

# SaaS platform AI — operator-provided, metered, billed AI compute per community

On 2026-06-23 we deliberately removed platform AI compute: the operator pointed
`RAG_EMBED_BASEURL`/`TRANSLATE_BASEURL` at a bundled Ollama and would have paid
for **every** community's AI compute. The fix made AI strictly **BYO
per-community** (own host + model + keys at `/settings`); the platform provides
**storage + messaging, not compute**. See
[[spec - saas-tenant-config - per-community owner-configurable ai rag translate storage and join policy]]
(`## Future` already anticipated `{[!]} Quotas/billing per tenant — embed
tokens, agent prompts/day`).

This spec **brings platform compute back as an explicit opt-in** — but only
behind the two guards that neutralise the original concern:

1. **Authorization.** A community uses platform compute only when a super-admin
   **granted** it (free / sponsored) or the owner holds an **active Stripe
   subscription**. Default stays BYO; unset community inherits nothing (inert,
   zero operator cost) — exactly as today.
2. **Metering.** Every request served by platform compute is recorded by **token
   count (in/out)** in an append-only ledger, dimensioned by community + feature
   (+ triggering user), so the operator can bill and analyse precisely where and
   how much their compute was spent.

It is the `## Future` "quotas/billing per tenant" chapter, made concrete for the
three AI subsystems: **RAG embedding, translation, agents.**

## Target

Let a community owner pay the operator (or be sponsored by the operator) to run
their AI on the platform's hosted compute instead of standing up their own
Ollama/Qdrant. Give the operator a hard cost-control and analytics surface so
"the platform pays for AI" never means "the platform eats unbounded cost."

Concretely, in `SAAS=true`:

- An owner can flip the community to **"use system-wide AI settings"** (one
  master switch routing RAG embed + translate + agents to platform compute).
- That switch only takes effect when the community is **authorized**: either a
  super-admin **granted** platform AI (free), or an **active Stripe
  subscription** covers it. Unauthorized → the switch is requested-but-inactive,
  AI stays BYO/off.
- Access is obtained owner→super-admin: the owner **requests** platform AI; the
  super-admin **approves free** (grant) or **approves-pending-payment** (owner
  completes Stripe checkout). Mirrors the existing self-serve community-creation
  request queue (`community_requests`, migration 00056).
- Every platform-compute request is **metered by token count** in an
  `ai_usage_events` ledger (community, feature, user, tokens_in, tokens_out,
  model, at). The operator sees per-community / per-feature usage and totals in
  `/superadmin`; the owner sees their own community's usage in `/settings`.

Non-targets (this spec): metered/usage-based Stripe pricing (start with a flat
subscription; the token ledger is ready for usage-based later), hard
auto-enforced quotas (the ledger + a soft cap warning is enough first), changing
**BYO** behaviour at all (BYO communities are never metered, never billed,
unchanged), and anything in `SAAS=false` self-hosted mode (no-op, as always).

## Behaviour

### The resolution rule gains a platform tier

Today the one resolver (`internal/community/resolve.go`) is
`effective = community.override ?? env.default`, gated by `env.kill_switch`. This
spec inserts **one** new precedence tier above the override:

```
effective(feature, community) =
  if !cfg.SAAS:                env.default                  # self-hosted, unchanged
  else if kill_switch off:     OFF                          # platform fleet-wide disable
  else if community uses platform AI AND authorized:
                               PLATFORM compute  + METER     # operator's hosted config
  else:                        community.override ?? env.default   # BYO, as today
```

- The platform tier is reached **only** when `SAAS=true`, the community's
  `use_platform_ai` master is on, **and** the community is authorized
  (granted-free OR active subscription). Any of those false → falls straight
  through to today's BYO/override path. So a single-tenant deploy and every
  BYO community behave byte-for-byte as before.
- "Authorized" is computed once: `granted_free OR subscription_status == active`.
  A canceled/past-due subscription with no grant → unauthorized → the community
  silently reverts to BYO/off (and the owner sees a "subscription lapsed" notice,
  not a broken feature).

### Platform compute config — re-introduced, but namespaced and inert by default

The platform needs its own hosted AI config to serve opted-in communities. We
re-add it under a **distinct `PLATFORM_AI_*` namespace** (NOT the BYO
`RAG_EMBED_*`/`TRANSLATE_*` env, which stay community-inheritance defaults):

- `PLATFORM_AI_RAG_BASEURL`, `PLATFORM_AI_RAG_MODEL`, `PLATFORM_AI_RAG_DIM`,
  `PLATFORM_AI_QDRANT_URL`, `PLATFORM_AI_QDRANT_API_KEY`
- `PLATFORM_AI_TRANSLATE_BASEURL`, `PLATFORM_AI_TRANSLATE_MODEL`
- `PLATFORM_AI_AGENT_PROVIDER`, `PLATFORM_AI_AGENT_BASEURL`,
  `PLATFORM_AI_AGENT_MODEL` (text, e.g. `glm-5.2`),
  `PLATFORM_AI_AGENT_VISION_MODEL` (vision, e.g. `gemma4`),
  `PLATFORM_AI_AGENT_API_KEY`

Unset → platform AI is simply unavailable (the super-admin can't grant what isn't
configured; an owner's request stays pending). Setting them is what the operator
does once to "open for business." This keeps the 2026-06-23 invariant intact for
everyone who doesn't opt in: **no platform AI env set OR no community opted in ⇒
operator pays zero.**

### Agents on platform compute

- Today each agent is BYO (`ai_agents.provider/base_url/model/api_key_enc`). When
  a community runs on platform AI, its agents' **generation** routes to
  `PLATFORM_AI_AGENT_*` (provider/host/key) instead of the agent's own config.
  The agent's **identity** (name, avatar, system_prompt, tools) is unchanged —
  only the compute backend is swapped.
- This is a community-level decision (the one master switch), not per-agent: "use
  system settings" means all of the community's AI uses platform compute.
- **Two platform models, picked by capability.** A vision agent forwards images
  to the model, and a text-only model errors on image input — so the platform
  offers a **text** model (`PLATFORM_AI_AGENT_MODEL`) and a **vision** model
  (`PLATFORM_AI_AGENT_VISION_MODEL`). `ResolveAgent(s, cfg, vision)` returns the
  vision model for a vision-capable agent, the text model otherwise. If a vision
  agent is requested but no platform vision model is configured, that agent stays
  **BYO** (never route images to a text model) rather than silently degrading.
- **The `/summary` summarizer routes to the vision model.** A channel summary can
  include image messages, so the `IsSummarizer` agent is resolved with
  `vision = a.Vision || a.IsSummarizer` → it uses the platform vision model (and
  falls back to BYO if none is configured). This covers the synchronous
  `Service.SummarizeToThread` path as well as the streaming pane.
- **One seam, three call paths.** Agent generation happens in three places — the
  streaming pane (`agent.Runner`), the synchronous `/summary`
  (`agent.Service.SummarizeToThread`), and forum-thread bots
  (`chatagents.ThreadRunner`). All three take the same `agent.ComputeResolver`
  closure (wired once in `main.go`), which returns the metered platform provider
  + the model-overridden agent, or the agent on its bare BYO provider. The
  returned agent (not the input) drives the generation, since the streamed model
  name comes from `Agent.Model`.

### RAG embedding on platform compute

- The embed worker (`internal/rag/worker.go:59`) and embedder resolver
  (`Service.EmbedderFor`) build the **platform** embedder + target the **platform
  Qdrant** collection for an opted-in community, instead of the community's BYO
  embedder. Per-community collection isolation (`forumchat_<id>`) is unchanged.
- A community switching BYO↔platform changes its embed model/dim → same "reindex
  on model change" rule the tenant-config spec already defines (drop collection +
  re-enqueue).

### Translation on platform compute

- The `/translate` composer (`internal/agent/translate.go`) resolves the
  **platform** translate base_url/model for an opted-in community.

### Metering — an append-only token ledger, written by a decorator

- A request served by platform compute writes one **`ai_usage_events`** row:
  `(id, community_id, feature, user_id NULL, model, tokens_in, tokens_out,
  request_count, created_at)`. `feature ∈ {agent, rag_embed, translate}`.
- **Token counts (in/out)** are the headline metric (the chosen grain). Agent
  generation gets real counts from the provider; embeddings/translation that
  don't return usage record an **estimate** (tokenizer or `len/4` heuristic),
  flagged `estimated=1` so analytics can distinguish.
- **Feature + community** dimensions are mandatory (they are the literal answer
  to "where & how much"). **Per-user attribution** (`user_id`) is recorded when a
  human triggered the request (chat send, translate click, agent prompt); NULL
  for background work (outbox embed of historical content). Super-admin-triggered
  requests are still metered to the community (the cost is real).
- **Only platform-compute requests are metered.** BYO requests write nothing —
  the ledger == the operator's cost == the bill. This is guaranteed structurally
  (below), not by remembering to call a recorder at each site.
- The ledger is **append-only**; aggregates (per community/feature/day, running
  totals) are computed by query, not mutated in place.

### Who sees usage

- **Super-admin** (`/superadmin`): a usage panel — per-community totals
  (tokens, request counts, by feature), sortable, with a date range; the
  operator's cost dashboard. Plus the grant/approve controls.
- **Owner** (`/c/{slug}/settings`): their own community's usage (this month,
  by feature) + subscription status + a "request / manage platform AI" card.

### Request → approve → (pay) lifecycle

```
owner: toggle "use system AI" → POST request           (status=requested)
super-admin: sees request in /superadmin
   ├─ Grant free      → granted_free=1, active          (sponsored)
   └─ Approve (paid)  → status=approved_unpaid
        owner: Stripe checkout → webhook subscription.active → active
super-admin or owner: revoke / cancel → subscription canceled OR grant removed
   → unauthorized → community reverts to BYO/off
```

- Reuses the **`community_requests`** precedent (owner requests something,
  super-admin approves) rather than inventing a new queue shape.

## Design

Follows the codebase's loader/closure seams (leaf packages never import
`community`), the §6b CQRS split, and the existing `secretbox` for secrets.

### Metering = a decorator over the platform Provider / Embedder / Translator

The load-bearing simplification. Instead of editing every AI call site to "record
usage," wrap the platform compute clients in metering decorators, installed
**only** on the platform-compute branch of the resolver:

```go
// internal/aiusage (new leaf package, no community/rag/agent imports)
type Event struct {
    CommunityID, Feature, UserID, Model string
    TokensIn, TokensOut int
    Estimated bool
}
type Recorder interface { Record(ctx context.Context, e Event) }  // async, best-effort

// decorators live next to each subsystem's interface:
//   agent.meteredProvider{Provider, rec, communityID, userID}  → records on Stream
//   rag.meteredEmbedder{Embedder, rec, communityID}            → records on Embed
//   agent.meteredTranslator{...}                               → records on Translate
```

- Each decorator implements the **same interface** it wraps (accept-interfaces),
  so callers are untouched. `main.go` builds the platform client, wraps it with
  the recorder, and hands the wrapped one down the existing closure seams.
- BYO communities get the **bare** client (no decorator) → no rows. The branch
  that chooses platform-vs-BYO is the single resolver, so "meter iff platform"
  is enforced in one place.
- `Recorder` is an interface (declared at the consumer) with a SQLite-backed impl
  (`aiusage.SQLRecorder`) wired in `main.go`. Writes are async/buffered so a slow
  ledger insert never blocks a generation (mirrors `debuglog.Recorder` nil-safe
  no-op shape).

### Surfacing token usage from the provider (the real cost)

`Provider.Stream` (`internal/agent/provider.go:42`) currently discards usage:
`ollamaChatChunk` (provider.go:116) parses only `Message`/`Done`/`Error`. To meter
real tokens:

- Add `Usage struct { PromptTokens, CompletionTokens int }` to `StreamResult`
  (provider.go:31).
- Parse Ollama's final `done:true` object fields `prompt_eval_count` +
  `eval_count` into it (one struct-field addition + assignment).
- `agent.Generate` (`internal/agent/generate.go:27`) accumulates per-turn usage
  across the agentic loop and returns it; the metered provider decorator reads it.
- Embeddings: Ollama `/api/embeddings` returns **no** token count → estimate from
  input length (`estimated=1`). Translation: same estimate path.
- When the Claude/OpenAI providers land (already anticipated), they return exact
  usage and slot into the same `StreamResult.Usage` field.

### Data model

`ALTER TABLE community_settings` (the tenant-config table, migration 00055) —
platform-AI columns, all nullable/default-off (consistent with "unset ⇒ env"):

```
use_platform_ai            INT NULL,      -- owner master switch (request intent)
platform_ai_status         TEXT NULL,     -- '' | requested | approved_unpaid | active | canceled
platform_ai_granted_free   INT NULL,      -- super-admin sponsorship (bypasses Stripe)
stripe_customer_id         TEXT NULL,
stripe_subscription_id     TEXT NULL,
stripe_subscription_status TEXT NULL,     -- mirror of Stripe (active|past_due|canceled|...)
platform_ai_requested_at   INT NULL
```

New migration **00059 — `ai_usage_events`** (append-only ledger):

```
ai_usage_events(
  id TEXT PRIMARY KEY,
  community_id TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
  feature TEXT NOT NULL,                  -- agent | rag_embed | translate
  user_id TEXT NULL REFERENCES users(id) ON DELETE SET NULL,
  model TEXT,
  tokens_in INTEGER NOT NULL DEFAULT 0,
  tokens_out INTEGER NOT NULL DEFAULT 0,
  estimated INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
-- index (community_id, created_at) for the per-community date-range rollups
```

New migration **00060 — `community_settings` platform-AI columns** (above).
(Numbers follow 00058; confirm at write time.) `cascade` on community delete is
already handled by `provision.Service.Delete`; the ledger rows cascade too. Data
export (`internal/dataexport`) **excludes** `ai_usage_events` (platform billing
property, not tenant content — same rule as agent prompts / RAG vectors, §5h).

### Authorization helper — one function

`internal/community/resolve.go` gains `PlatformAI(s Settings, cfg) (on bool,
authorized bool)`: `on = cfg.SAAS && boolOr(s.UsePlatformAI,false)`,
`authorized = s.PlatformAIGrantedFree || s.StripeSubscriptionStatus=="active"`.
`ResolveRAG`/`ResolveTranslate`/agent-provider-resolution each check
`on && authorized` first and return the **platform** config (sourced from the new
`PLATFORM_AI_*` cfg fields) before the existing override/env logic.

### Billing — `internal/billing` (Stripe)

A new leaf package wrapping `stripe-go`:

- `POST /c/{slug}/settings/billing/checkout` (owner) → creates a Stripe Checkout
  Session for the platform-AI subscription product; redirects to Stripe.
- `POST /billing/webhook` (public, **untrusted external input** — signature
  verified with the webhook secret) → on `customer.subscription.*` events updates
  `community_settings.stripe_subscription_status` + `platform_ai_status`.
- The webhook is the only authority on subscription state; the app never trusts a
  client-reported "I paid." Signature verification (HMAC) + idempotency by Stripe
  event id (a `stripe_events` dedup table) + a stale-subscription guard (ignore a
  lifecycle event for a subscription that is no longer the community's current
  one, so a late old-sub `deleted` can't deactivate a live one). Transient
  storage failures return 5xx so Stripe retries; "not ours / unknown customer"
  returns 200. (This handler is the security-review gate at implementation time —
  Codex/`/codex:review` before merge; the first pass already folded in
  replay/out-of-order/lost-event findings.)
- **Granting statuses:** platform AI is authorized when the subscription status is
  `active` **or** `trialing` (`community.SubscriptionGrantsAccess`); `past_due`,
  `canceled`, `unpaid`, `incomplete`, `paused` do not grant — the resolver then
  reverts the community to BYO/off.
- Stripe keys are env (`STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`,
  `STRIPE_PLATFORM_AI_PRICE_ID`); platform-AI billing is unavailable if unset.

### UI (datastar)

- **Owner `/c/{slug}/settings`** — a new **Platform AI** card (stable-id
  live-morph, §4.7): the "use system-wide AI settings" toggle, current status
  (requested / sponsored / active / lapsed), a "Subscribe" button (→ Stripe
  checkout) when approval requires payment, and a **usage summary** (this month,
  by feature). Sits with the existing AI/RAG/translate cards but is the
  platform-vs-BYO selector above them.
- **Super-admin `/superadmin`** — a **Platform AI** section: pending requests
  (Grant free / Approve-paid / Deny), a per-community usage table (tokens +
  requests by feature, date range), and grand totals. Built like the existing
  `SADebugCard` shared-id morph.

### Boot wiring (`cmd/app/main.go`)

- Build `aiusage.SQLRecorder` and the platform AI clients (provider, embedder,
  translator) from `PLATFORM_AI_*` env; wrap them in metering decorators.
- The per-community resolver closures (already handed to `rag`, translate,
  agent) choose platform-wrapped vs BYO-bare client based on `PlatformAI(...)`.
- Mount billing routes + webhook only when Stripe env is set; mount the
  super-admin usage panel always (it just shows zero until someone opts in).

## Verification

- **Resolver tier:** table test `PlatformAI` + `Resolve*` — (SAAS off → env),
  (kill-switch off → off), (platform on + authorized → platform config + meter),
  (platform on + unauthorized → BYO/override), (platform off → BYO/override).
- **Decorator meters iff platform:** a platform-wrapped provider records exactly
  one `ai_usage_events` row per generation with the right tokens; a BYO bare
  provider records zero. Same for embedder + translator.
- **Token surfacing:** an Ollama `done` object with `prompt_eval_count=N`,
  `eval_count=M` yields `StreamResult.Usage{N,M}`; `Generate` sums across a
  multi-turn tool loop.
- **Authorization transitions:** granted_free → authorized; grant removed +
  no sub → unauthorized → next request takes BYO/off path, no new ledger rows.
  Subscription active → authorized; webhook canceled → unauthorized.
- **Stripe webhook:** a forged signature is rejected; a valid
  `subscription.active`/`canceled` updates status; replay of the same event id is
  idempotent. (Security review before merge.)
- **Ledger integrity:** export excludes `ai_usage_events`; community delete
  cascades it; rollup query returns correct per-feature totals over a range.
- **Mode isolation:** `SAAS=false` → none of this mounts, no platform clients
  built, existing tests green. `SAAS=true` with no `PLATFORM_AI_*` → grant is
  impossible, requests stay pending, zero cost.
- **Smoke:** `make gen && make build && make test`; SaaS HTTP smoke — owner
  requests platform AI, super-admin grants free, owner sends an agent prompt,
  one `ai_usage_events` row appears with token counts, owner + super-admin usage
  panels show it.

## Friction

- **Token usage is provider-dependent.** Ollama gives real chat counts but **no**
  embedding/translation counts → those are estimates (`estimated=1`). Be explicit
  in analytics that estimated rows are not exact. Hosted providers (Claude/OpenAI)
  will give exact counts everywhere later.
- **This reverses a documented decision.** The 2026-06-23 "platform = storage, not
  compute" rule still holds for everyone who doesn't opt in; document that
  platform compute is an **authorized, metered exception**, not the default. The
  `PLATFORM_AI_*` namespace (separate from BYO `RAG_*`/`TRANSLATE_*`) is what keeps
  the default inert.
- **Stripe is real money + untrusted webhooks.** Signature verification,
  idempotency, and treating the webhook as the sole source of truth are
  mandatory; this is the security-review surface.
- **Soft cap, not hard quota (v1).** Without enforcement an authorized community
  could run up cost between billing cycles. Mitigate with a per-community monthly
  **soft cap** (warn + optionally suspend platform AI when exceeded) reading the
  same ledger; hard metered billing is a later phase.
- **Decorator must not block generation.** Recorder writes are async/best-effort;
  a ledger write failure logs but never fails the user's request (consistency of
  billing data is eventually-correct, not transactional with the generation).
- **BYO↔platform model swap invalidates RAG vectors** (different embed dim) — same
  reindex friction the tenant-config spec already documents; surface it on the
  toggle.

## Interactions

- Extends [[spec - saas-tenant-config - per-community owner-configurable ai rag translate storage and join policy]]
  — this is its `## Future` "quotas/billing per tenant" item, made concrete; adds
  the platform tier above its `override ?? env` resolver.
- Reworks compute resolution in the RAG subsystem (`internal/rag`) and agent
  generation (`internal/agent`) — adds a platform-wrapped client path.
- Reuses the request→approve queue of the self-serve community-creation flow
  (`internal/community/requests.go`, migration 00056).
- Builds on the platform super-admin surface (§5d) for grant/approve + usage.
- Excluded from [[spec - data-export - owner-initiated full tenant data export to signed-url zip]]
  (billing ledger is platform property, not tenant content).
- New external dependency: Stripe (`stripe-go`).

## Mapping

> [[internal/config/config.go]]
> [[internal/community/resolve.go]]
> [[internal/community/settings.go]]
> [[internal/community/requests.go]]
> [[internal/aiusage/aiusage.go]]
> [[internal/aiusage/recorder.go]]
> [[internal/billing/billing.go]]
> [[internal/billing/webhook.go]]
> [[internal/agent/provider.go]]
> [[internal/agent/generate.go]]
> [[internal/agent/translate.go]]
> [[internal/rag/embedder.go]]
> [[internal/rag/worker.go]]
> [[internal/rag/service.go]]
> [[internal/superadmin/handler.go]]
> [[internal/admin/settings.go]]
> [[internal/storage/sqlite/migrations/00059_ai_usage_events.sql]]
> [[internal/storage/sqlite/migrations/00060_platform_ai_settings.sql]]
> [[cmd/app/main.go]]

## Decisions

- {>>} Billing model: super-admin can grant platform AI **free** to any community;
  otherwise an **active Stripe subscription** is required. (User decision,
  2026-06-24.)
- {>>} Metering grain: **token counts (in/out)**, dimensioned by feature +
  community (+ user when human-triggered); meter **only** platform-compute
  requests. (User decision, 2026-06-24.)
- {>>} Toggle authority: owner **requests**, super-admin **approves** (grant-free
  or approve-paid). (User decision, 2026-06-24.)

## Future

- {[!]} Usage-based Stripe metered billing (report `ai_usage_events` token sums to
  Stripe metered prices) — the ledger is already shaped for it.
- {[!]} Hard per-community quotas with auto-suspend (the `agentlimit` per-community
  rate-limit precedent + the ledger).
- {[?]} Per-feature platform pricing tiers (RAG cheaper than agent generation).
- {[?]} Exact embedding/translation token counts once hosted providers replace the
  estimate path.
- {[?]} Cost→revenue dashboard (operator's actual provider bill vs subscription
  revenue per community).

## Notes

- The **decorator-on-the-platform-branch** is the load-bearing design: it makes
  "meter exactly the requests the operator pays for" a structural property, not a
  discipline. Resist sprinkling `recorder.Record(...)` at call sites.
- Keep `PLATFORM_AI_*` env **separate** from the BYO `RAG_*`/`TRANSLATE_*` env so
  the 2026-06-23 "inert by default, operator pays zero" invariant survives for
  every non-opted-in community.
