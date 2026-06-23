---
name: spec-saas-tenant-config-per-community-owner-configurable-infra
status: draft
type: spec
tldr: When SAAS=true, each community becomes a self-serve tenant. A new per-community `owner` role (community super-admin, above `admin`) configures the tenant's own AI agents + ai_enabled, RAG embed model/host + a dedicated Qdrant collection with dynamic vector size, translation model/host, file storage (shared platform S3 namespaced by community, with an opt-out to migrate to the community's own bucket), and the join policy (open vs request-approval). SMTP stays a single global relay; IMAP mailbox is disabled in SaaS. Endpoints/keys are BYO per community, encrypted at rest, falling back to platform env defaults. Self-hosted (SAAS=false) keeps today's single-tenant global config untouched.
---

# SaaS tenant config — per-community owner-configurable infrastructure

Today every cross-cutting capability is wired **once, globally, from env**: one
`RAG_*` embedder + one vector store partitioned by `community_id` metadata
(`internal/rag/rag.go:79`), one `TRANSLATE_*` daemon (`config.go:222`), one
disk-only `uploads.Store` (`internal/uploads/uploads.go:151`), one `AI_ENABLED`
instance flag gating every community's agents, one global `SMTP_*` relay and one
global `MAILBOX_*` IMAP poller. That is exactly right for a **self-hosted,
single-tenant** deployment — one operator, one trust boundary, one set of
daemons.

It breaks down the moment forumchat is run as **SaaS** (managed helpdesk for many
unrelated client communities on one instance). There, each client community is
its own tenant and wants to choose its **own** AI model, its **own** Ollama host,
its **own** RAG embedding model (hence its own vector dimensionality), its **own**
translation model, control whether AI is on at all, decide whether its docs live
in the shared platform bucket or a private bucket it owns, and decide whether the
public can join freely or must be approved — all **without** a redeploy and
**without** the platform operator touching env.

This spec makes those capabilities **per-community, owner-configurable** when
`SAAS=true`, while leaving the single-tenant path byte-for-byte unchanged.

## Target

Turn a community into a **self-serve tenant** in SaaS mode. The unit of
configuration is the community; the authority is a new community-level **owner**
role. Concretely, in SaaS mode a community owner can, from the community's own
admin surface (no platform operator, no redeploy):

- Toggle **AI** on/off for the community and add/enable/disable its **agents**
  (the agents themselves are already per-community; this adds the master switch
  and the owner gate).
- Configure **RAG**: enable it, pick the embedding **model** + **Ollama host**,
  which fixes a **vector dimensionality**, backed by a **dedicated Qdrant
  collection** for that community (isolation, cheap per-community delete/reindex).
- Configure **translation**: enable it, pick the model + Ollama host.
- Choose **file storage**: use the shared platform store (namespaced by community)
  or **migrate to the community's own S3 bucket** (privacy opt-out), moving
  existing objects.
- Set the **join policy**: `open` (anyone may join, auto-approved) vs `request`
  (join lands in the approval queue).

Non-targets: per-community SMTP (mail stays one global relay — owners cannot set
mail servers), IMAP in SaaS (disabled — inbound mail ingest is a single-tenant
feature), multi-region, billing/quotas (separate concern), and changing anything
about the self-hosted single-tenant experience.

## Behaviour

### Modes

- **`SAAS=false` (self-hosted, default).** Nothing changes. Every capability
  reads its global `*_ENABLED` flag and global `*_BASEURL/_MODEL/...` env exactly
  as today. The per-community config surfaces are **hidden**; the `owner` role is
  inert (an existing admin is the de-facto top role). No new required env.
- **`SAAS=true`.** Communities are tenants. Registration is **open** (strangers
  can sign up and create/join communities — this is the existing
  `OPEN_REGISTRATION` semantics, implied on in SaaS). The per-community config
  surfaces appear for owners. Global env values become **platform defaults /
  fallbacks**, not the only option. IMAP is force-disabled regardless of
  `MAILBOX_ENABLED`.

### Roles — `owner` is the new community super-admin

A new role **`owner`** ranks above `admin` (`member < moderator < admin < owner`).

| Capability | member | mod | admin | owner | platform super-admin |
|---|:--:|:--:|:--:|:--:|:--:|
| Read/post, normal use | ✓ | ✓ | ✓ | ✓ | ✓ |
| Moderate content, ban | — | ✓ | ✓ | ✓ | ✓ |
| Approve members, set roles ≤ admin, manage agents' *content* | — | — | ✓ | ✓ | ✓ |
| **Configure infra**: ai_enabled, RAG model/host/collection, translate model/host, storage backend/migration, join policy, secrets | — | — | — | ✓ | ✓ |
| Set/transfer `owner`, delete community | — | — | — | ✓ | ✓ |
| Cross-community god mode, /superadmin | — | — | — | — | ✓ |

- Exactly one capability boundary is new: **infra config is owner-gated**
  (`RequireRole(RoleOwner)`), everything admins do today stays admin-gated.
- The community **creator** becomes its `owner`. Existing communities: a
  migration promotes the **earliest-created admin** of each community to `owner`
  (deterministic, one per community); communities with no admin are left
  owner-less and an operator seeds one via CLI.
- The platform super-admin's synthetic membership is bumped to `owner` so god
  mode still passes the new gate (`auth.SuperAdminMembership`).
- CLI `forumchat-cli role <email> owner` and a new `forumchat-cli owner <slug>
  <email>` seed/transfer ownership for bootstrap.

### Per-community config — resolution rule (one rule everywhere)

Every configurable value resolves the same way: **per-community override if set,
else platform env default.** A capability is **on** for a community when its
per-community enable flag is on (SaaS) — falling back to the global `*_ENABLED`
flag when the community hasn't configured it. The global `*_ENABLED` flag in
SaaS mode acts as a **platform kill-switch**: off globally ⇒ off for everyone,
no per-community override can turn it on (so the operator can disable a feature
fleet-wide). On globally ⇒ each community decides.

This gives a single mental model: `effective = community.override ?? env.default`,
gated by `env.kill_switch`.

### AI + agents

- New per-community **`ai_enabled`**. Effective AI = `env.AI_ENABLED` (kill
  switch) AND community `ai_enabled` AND ≥1 enabled agent (the existing
  condition). In self-hosted mode `ai_enabled` defaults on so behaviour is
  unchanged.
- Agents are already per-community (`ai_agents`: provider/base_url/model/key/
  system_prompt/enabled). This spec only moves their **admin surface** under the
  owner gate in SaaS and adds the master toggle. Agent *content* management
  (create/edit a bot, its prompt) can stay admin-gated; turning the **feature**
  on/off is owner-gated.

### RAG — per-community embedder + dedicated Qdrant collection, dynamic dim

This is the structural heart. Today RAG is one embedder (one model → one fixed
`RAG_EMBED_DIM=1024`) and one store (single chromem collection partitioned by a
`community_id` metadata field). Per-community models mean **per-community vector
dimensionality**, and vectors of different dims cannot share one collection — so
the store must become **per-community collections**.

- Per-community RAG config: `rag_enabled`, `embed_base_url`, `embed_model`,
  `embed_dim`, `qdrant_url`, `qdrant_api_key` (encrypted), `qdrant_collection`
  (defaults to `forumchat_<communityID>`).
- **Qdrant Store implemented** (it is reserved-not-built today). One **collection
  per community**, created on demand with `size = community.embed_dim`,
  `distance = Cosine`. Per-community delete = drop the collection (cheap,
  isolated); per-community reindex = recreate + re-enqueue.
- The `rag.Store` interface already takes `communityID` on `Upsert`/`Query`/
  `DropCommunity` — the change is that the implementation routes to the
  community's collection and the **embedder is resolved per community** (its
  model/host/dim), not a single process-wide embedder. The worker, when draining
  `embed_outbox`, groups jobs by community and uses that community's embedder +
  collection.
- **chromem stays** as the single-tenant backend (self-hosted): one process, one
  embedder, the existing single-collection-by-metadata design is fine there. The
  per-community-collection behaviour is the **qdrant** backend, selected in SaaS.
- Dimensionality is **dynamic**: bge-m3 → 1024, e5-large → 1024, nomic →768,
  gte-large →1024, a 4096-dim model →4096, MiniLM →384. The community's
  `embed_dim` is authoritative and stored alongside the model so a model swap is
  an explicit "reindex with new dim" (drop + recreate collection at the new size).
- Changing a community's embed model/dim **invalidates its vectors** — the owner
  action is "save + reindex this community", which drops the collection and
  re-enqueues all of that community's content.

### Translation

- Per-community `translate_enabled`, `translate_base_url`, `translate_model`. The
  `/translate` composer reads the **community's** config (resolved via the rule),
  not the global one. Self-hosted falls back to global env.

### File storage — shared, namespaced, with per-community S3 opt-out

- Extract a **`Blobstore`** interface (Put / Open / Remove / Stat / Exists) from
  today's disk-only `uploads.Store`. The `uploads.Store` keeps the DB metadata,
  signing, MIME logic, dedup; only the **bytes backend** becomes pluggable.
- Backends: **`disk`** (current behaviour) and **`s3`** (S3-compatible:
  AWS/MinIO/R2). Instance-wide default via `STORAGE_BACKEND` env.
- **Namespacing:** object keys are prefixed by community id
  (`<communityID>/<sha2>/<sha>.<ext>`) so the shared platform bucket cleanly
  separates tenants and a community delete can prune its prefix.
- **Per-community S3 opt-out (SaaS):** a community owner can point storage at the
  community's **own** bucket (BYO endpoint/region/bucket/credentials, encrypted).
  Switching triggers a **migration job** that copies the community's existing
  objects from the platform store to the community store, flips the pointer, and
  (optionally) prunes the originals. New uploads route to the community store;
  reads resolve per-upload via the store recorded at write time.
- Self-hosted: one global `disk` (or one global `s3`) — no per-community buckets,
  the opt-out UI is hidden.

### Join policy — public vs request-approval

- `is_public` already exists (discoverability in `/explore`). This adds an
  orthogonal **`join_policy`**: `open` (join → membership auto-approved, straight
  in) vs `request` (join → `approved_at = NULL` → `/pending` → owner/admin
  approves), settable by owner/admin.
- A community can be public+open (anyone discovers and joins), public+request
  (discoverable, must be approved), or private (invite-only, `join_policy`
  irrelevant — there is no public join).
- In SaaS, registration is open globally; the **community** decides its own gate.

### Mail / IMAP

- **SMTP stays one global relay.** Owners cannot configure mail. (Transactional
  email — verify, magic link, invites — is a platform concern.)
- **IMAP is disabled in SaaS.** `MAILBOX_ENABLED` is ignored (forced off) when
  `SAAS=true`; the inbox routes and worker don't mount. It remains available for
  single-tenant self-hosted deployments.

### Secrets at rest

Per-community BYO endpoints carry secrets (Qdrant API key, S3 credentials, future
hosted-LLM keys). A small **`secretbox`** helper (AES-GCM, key from
`SECRETS_KEY`) encrypts them before they hit SQLite; columns are `*_enc`. In dev
with no `SECRETS_KEY`, values are stored with a plaintext sentinel so local dev
keeps working; **prod boot rejects** SaaS mode without `SECRETS_KEY` (same shape
as the existing `SESSION_KEY`/`UPLOADS_SIGN_KEY` guards in `config.Load`).

## Design

Follows the codebase's CQRS-ish split (§6b) and the existing per-community
patterns: `ai_agents` (per-community config rows), `ai_configs` (the original
singleton it replaced), and the loader/closure seams that keep leaf packages
decoupled.

### Data model — `community_settings` (migration 00055) + role + storage column

One **`community_settings`** row per community (PK `community_id`, FK cascade),
all columns nullable so "unset ⇒ fall back to env". Grouping all tenant config in
one table keeps the resolution code in one place and the JOINs cheap:

```
community_settings(
  community_id PK FK,
  -- AI
  ai_enabled INT NULL,
  -- RAG
  rag_enabled INT NULL,
  rag_embed_base_url TEXT NULL,
  rag_embed_model TEXT NULL,
  rag_embed_dim INT NULL,
  rag_qdrant_url TEXT NULL,
  rag_qdrant_api_key_enc TEXT NULL,
  rag_qdrant_collection TEXT NULL,
  -- translate
  translate_enabled INT NULL,
  translate_base_url TEXT NULL,
  translate_model TEXT NULL,
  -- storage
  storage_backend TEXT NULL,          -- '' | 'disk' | 's3'
  storage_s3_endpoint TEXT NULL,
  storage_s3_region TEXT NULL,
  storage_s3_bucket TEXT NULL,
  storage_s3_access_key_enc TEXT NULL,
  storage_s3_secret_key_enc TEXT NULL,
  storage_migrated_at INT NULL,
  -- join
  join_policy TEXT NULL,              -- '' | 'open' | 'request'
  updated_at INT
)
```

- `ALTER TABLE memberships` — no schema change needed; `role` is free-text TEXT,
  so `owner` is just a new value. The migration **promotes** the earliest admin
  per community to `owner`.
- `ALTER TABLE uploads ADD COLUMN store_key TEXT` — records which store an upload
  lives in (`''`/`disk`/`s3:<bucket>`), so reads resolve the right backend after
  a community migrates buckets. Default `''` = the legacy/global store.
- `join_policy` could equally live on `communities`; it sits in
  `community_settings` to keep all tenant policy together. `is_public` stays on
  `communities` (already there, already indexed by `/explore`).

### Resolution — `community.Settings` + a resolver, single source of truth

- `internal/community/settings.go`: `Settings` struct (pointers / sql.Null for
  "unset"), `Repo.Settings(ctx, communityID)`, `Repo.SaveSettings(...)`.
- A **resolver** turns `(Settings, config.Config)` into the **effective**
  values each subsystem consumes — e.g. `ResolveRAG(s, cfg) → rag.CommunityConfig`,
  `ResolveTranslate`, `ResolveStorage`, `EffectiveAIEnabled`, `JoinPolicy`. The
  resolver is the **one** place the `override ?? default`-gated-by-kill-switch
  rule lives. In `SAAS=false` it short-circuits to env (settings ignored), which
  is what keeps single-tenant behaviour identical.
- Accept-interfaces: each subsystem depends on a tiny local interface it's handed
  (e.g. `rag` gets a `CommunityConfigFunc func(ctx, communityID) (CommunityConfig,
  error)` closure wired in `main.go`, exactly like `chat.ListProjects` /
  `agent.ShareToChannel`). No subsystem imports `community` directly to avoid
  cycles.

### RAG per-community — the worker fans out by community

- `rag.Embedder` stays the interface; add a per-community **resolver**: the
  service/worker, given a job's `community_id`, builds (or caches) that
  community's `Ollama` embedder from its `embed_base_url/embed_model/embed_dim`.
- `rag.Store` qdrant impl: `collectionFor(communityID)` ensures a collection sized
  to that community's `embed_dim`; `Upsert`/`Query` target it; `DropCommunity`
  drops it. A small LRU/`map` caches resolved embedders + ensured collections.
- `Worker.tick` groups the dequeued batch by community and processes each group
  with that community's embedder + collection. A community whose embedder is
  unreachable backs off without blocking others (today's single-embedder
  backpressure becomes per-community).
- Reindex (owner button + CLI) is already per-community (`ReindexCommunity`); it
  now drops the community's **collection** (qdrant) before re-enqueue.

### Storage — `Blobstore` interface

```go
// internal/uploads/blob.go
type Blobstore interface {
    Put(ctx, key string, r io.Reader, size int64, mime string) error
    Open(ctx, key string) (io.ReadCloser, error)
    Remove(ctx, key string) error
    Exists(ctx, key string) (bool, error)
}
```

- `diskBlobs{dir}` — wraps today's `os` calls (the current `Store.Dir` logic).
- `s3Blobs{client, bucket, prefix}` — S3-compatible (minio-go or aws-sdk-v2).
- `uploads.Store` holds a **default** `Blobstore` + a resolver
  `blobstoreFor(ctx, communityID) Blobstore` (community's own S3 if migrated,
  else default). `Save`/`Get`/`Delete`/`PathFor`→`Open` route through it; the
  `store_key` column records where each object went so reads are unambiguous.
- The signed-URL handler streams via `Blobstore.Open` instead of `os.Open`.
  Signing/HMAC/expiry/stable-URL logic is untouched (it's about the upload id,
  not the bytes location).
- Migration job (`uploads.MigrateCommunity`): list the community's uploads, copy
  each object platform→community store, stamp `store_key`, set
  `storage_migrated_at`. Idempotent (skip already-migrated rows); resumable.

### UI (datastar) — owner config under `/c/{slug}/admin`

- A new owner-gated **"Settings"** section in the community admin area
  (`/c/{slug}/admin/settings`), rendered only when `SAAS=true` and viewer is
  owner/super-admin. Cards: **AI**, **RAG** (model/host/dim + "Reindex"),
  **Translate**, **Storage** (backend + "Migrate to my S3"), **Join policy**.
- Each card is a stable-id templ fragment patched on save (§4.7 live-morph).
  Secrets render as write-only password inputs (never echo the stored value).
- Owner-vs-admin: admins see the existing member/content admin; the Settings tab
  appears only for owners. `web/templ` reads owner-ness via the same ctx-key
  leaf-package trick as `AdminAnyCtxKey`/super-admin (§4.13).

### Boot wiring (`main.go`)

- Build the `secretbox` from `SECRETS_KEY`; pass it to `community.Repo` (settings
  decrypt) + `uploads` (S3 creds) + `rag` (qdrant key).
- The per-community resolver closures are wired here and handed to `rag`, the
  translate handler, `uploads`, and the AI gate — concrete types wired centrally,
  consumers depend on interfaces/closures.
- In `SAAS=true`: force `MailboxEnabled=false`, default `OpenRegistration=true`,
  mount the owner Settings routes. In `SAAS=false`: skip all of it.

## Verification

- **Resolution rule:** table test `Resolve*` — (override set → override),
  (override unset → env default), (env kill-switch off → off regardless of
  override). Self-hosted mode (`SAAS=false`) → always env, settings ignored.
- **Role:** `RoleOwner.AtLeast(RoleAdmin)` true, `RoleAdmin.AtLeast(RoleOwner)`
  false; `RequireRole(RoleOwner)` 403s an admin, passes an owner and a
  super-admin. Migration promotes exactly one admin→owner per community.
- **Secrets:** `secretbox` round-trip; a `*_enc` column never contains
  plaintext; prod+SaaS boot without `SECRETS_KEY` is rejected.
- **RAG qdrant:** against a local Qdrant (or a fake) — two communities with
  different `embed_dim` get two collections of the right size; a query in
  community A never returns community B's chunks; `DropCommunity` removes only
  A's collection; changing A's model → reindex recreates A's collection at the
  new dim.
- **Storage:** `Blobstore` disk + s3 (MinIO or a fake) pass the same Put/Open/
  Remove/Exists contract; a signed URL streams identical bytes from either; a
  community migration copies objects, stamps `store_key`, and subsequent reads of
  pre-migration uploads still resolve.
- **Join policy:** `open` → join auto-approved (no `/pending`); `request` → join
  lands in queue; owner/admin approval flow unchanged.
- **Mode isolation:** with `SAAS=false`, none of the new routes mount, RAG/
  translate/storage read env exactly as before (existing tests stay green); with
  `SAAS=true`, IMAP routes/worker absent even if `MAILBOX_ENABLED=true`.
- **Smoke:** `make gen && make build && make test`; manual HTTP smoke on a fresh
  high port (§13) in SaaS mode — create a community (owner), set its RAG model,
  reindex, search; flip join policy and watch a second user's join land
  approved vs pending.

## Friction

- **Embedder/collection cache invalidation.** When an owner changes the embed
  model, cached embedders + the ensured-collection set must invalidate for that
  community or the worker keeps using the old dim. Tie invalidation to the save +
  reindex action; key caches by `(communityID, model, dim)`.
- **Qdrant availability.** Qdrant is now a hard dependency in SaaS RAG. If it's
  down, embedding backs off per community (good) but search returns empty (degrade
  gracefully, don't 500). Self-hosted chromem has no such dependency.
- **Storage migration is bytes-heavy and not transactional.** Copy-then-flip with
  `store_key` per row makes it resumable/idempotent; a half-migrated community
  reads correctly (each upload knows its own store). Pruning originals is a
  separate, after-verified step. Don't block the owner's request on the copy —
  run it as a background job with progress.
- **Kill-switch semantics must be documented** so an owner doesn't think the
  platform "broke" their AI when the operator flipped the global flag off.
- **`owner` everywhere super-admin was assumed top.** Audit every
  `Role.AtLeast(RoleAdmin)` and last-admin guard (`CountAdmins`,
  `RequireApproved`) — owner must satisfy them; "last owner" gets the same
  removal guard admins have.
- **Secrets key rotation** is out of scope; document that rotating `SECRETS_KEY`
  orphans existing ciphertext (owners re-enter secrets).

## Interactions

- Extends [[spec - forumchat - community web app with realtime chat and forum threads]]
  (the SaaS/landing flag, registration, communities).
- Reworks the global wiring of [[spec - agent - per-community-ai-chat-with-threads-and-resumable-streaming]]
  (adds the per-community `ai_enabled` master switch + owner gate; agents already
  per-community).
- Makes per-community what was global in the RAG subsystem
  (`internal/rag`, migration 00039) — the same authorization-in-the-loaders rule
  holds, now with per-community collections.
- Storage refactor touches [[spec - chat-attachments - drag-anywhere-multi-mime-extract-to-project]]
  and projects/issues attachments (all go through `uploads.Store`).
- Disables [[spec - mailbox - imap-ingest-to-per-community-queue]] in SaaS mode.
- Builds on the platform super-admin surface (§5d) — owner is the per-community
  analogue; super-admin still bypasses all owner gates.

## Mapping

> [[internal/config/config.go]]
> [[internal/community/community.go]]
> [[internal/community/settings.go]]
> [[internal/auth/user.go]]
> [[internal/auth/middleware.go]]
> [[internal/auth/superadmin.go]]
> [[internal/rag/rag.go]]
> [[internal/rag/qdrant.go]]
> [[internal/rag/worker.go]]
> [[internal/rag/service.go]]
> [[internal/uploads/uploads.go]]
> [[internal/uploads/blob.go]]
> [[internal/agent/translate.go]]
> [[internal/secretbox/secretbox.go]]
> [[internal/storage/sqlite/migrations/00055_community_settings.sql]]
> [[cmd/app/main.go]]
> [[cmd/cli/main.go]]

## Future

- {[?]} Per-community SMTP (custom branded transactional mail) — deliberately out
  now; mail stays global.
- {[?]} Per-community hosted-LLM providers (Claude/OpenAI) — the BYO endpoint +
  `secretbox` + `api_key_enc` columns already anticipate it; add provider
  branches in `agent/provider.go`.
- {[!]} Quotas/billing per tenant (storage GB, embed tokens, agent prompts/day —
  `agentlimit` already exists per community).
- {[?]} Per-community Qdrant **cluster** (not just collection) for the largest
  tenants wanting full isolation.
- {[?]} Owner-initiated full tenant export/delete (GDPR) — collection drop +
  bucket prune + community cascade already give the primitives.

## Notes

- The resolution rule (`community.override ?? env.default`, gated by
  `env.kill_switch`) is the load-bearing simplification — implement and test it
  once, reuse it for every subsystem. Resist per-subsystem ad-hoc gating.
- `SAAS=false` MUST remain a no-op path: the cheapest correctness guarantee is
  that the resolver returns env directly when not in SaaS, so existing tests and
  deployments are untouched.
