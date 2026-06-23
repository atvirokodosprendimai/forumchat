---
name: plan-2606230830-saas-tenant-config
status: in-progress
type: plan
spec: spec - saas-tenant-config - per-community owner-configurable ai rag translate storage and join policy
tldr: Phased implementation of per-community owner-configurable infra in SaaS mode. Foundation (owner role + secretbox + community_settings + resolver) first, then join policy, storage Blobstore+S3, per-community AI/translate, per-community RAG+Qdrant. Each phase mergeable + verifiable; SAAS=false stays a no-op path.
---

# Plan — SaaS tenant config

Implements [[spec - saas-tenant-config - per-community owner-configurable ai rag translate storage and join policy]].

**Backends by mode (confirmed with user):** SaaS ⇒ Qdrant + S3 are the path
(defaults flip to them); single-tenant self-host ⇒ chromem-go + local `./uploads`
(unchanged). Per-community override exists in both, but the SaaS *defaults* are
qdrant/s3.

**Invariant across all phases:** `SAAS=false` must remain byte-for-byte the
current behaviour. The resolver short-circuits to env when not SaaS; new routes
don't mount; new columns are nullable and default to "fall back to env".

---

## Phase 0 — Foundations: owner role, secretbox, community_settings, resolver

Goal: the shared machinery every later phase needs. Mergeable on its own (adds a
role + a settings table + a resolver, wires nothing destructive).

Steps:

1. **Migration `00055_community_settings.sql`** (next after `00054`):
   - `CREATE TABLE community_settings(community_id PK FK cascade, … all nullable …, updated_at)` per spec Data model.
   - `ALTER TABLE uploads ADD COLUMN store_key TEXT NOT NULL DEFAULT ''`.
   - Backfill: promote the earliest-created `admin` per community to `owner`:
     `UPDATE memberships SET role='owner' WHERE id IN (SELECT earliest admin per community)`. One per community, deterministic by `created_at, id`.
   - verify: open a pre-00055 DB, migrate, assert exactly one `owner` per community that had an admin; `community_settings` empty (lazy-created).

2. **`auth.RoleOwner`** (`internal/auth/user.go:16-25`):
   - add `RoleOwner Role = "owner"`; rank `{member:0, mod:1, admin:2, owner:3}`.
   - `SuperAdminMembership` (`internal/auth/superadmin.go:38`) → `Role: RoleOwner`.
   - Audit `Role.AtLeast(RoleAdmin)` sites (`middleware.go:209`, `repo.go:629/643/894`, admin last-admin guard): owner must satisfy admin gates (it does via rank) — confirm none hard-compare `== RoleAdmin`. Add "last owner" guard mirroring `CountAdmins` if owner removal is exposed.
   - verify: `role_test.go` — `RoleOwner.AtLeast(RoleAdmin)` true, `RoleAdmin.AtLeast(RoleOwner)` false; `RequireRole(RoleOwner)` 403s admin, passes owner + super-admin.

3. **`internal/secretbox/secretbox.go`** — AES-256-GCM `Box{key}` with
   `Seal(plaintext) (string, error)` / `Open(cipher) (string, error)`; key from
   32-byte `SECRETS_KEY`. Empty key ⇒ `nopBox` (passthrough with a `plain:`
   sentinel) for dev. verify: round-trip test + `nopBox` passthrough test.

4. **`config.Config`** (`internal/config/config.go`):
   - add `SecretsKey string env:"SECRETS_KEY"`, `StorageBackend string env:"STORAGE_BACKEND" default ""` (resolved: SaaS→s3, else disk).
   - In `Load()` (`config.go:239`): when `SAAS=true` → force `MailboxEnabled=false`, default `OpenRegistration=true` (if unset); when `IsProd() && SAAS` and `SecretsKey==""` → reject (mirror `SESSION_KEY` guard at `config.go:246`).
   - verify: `config` test — SaaS forces mailbox off; prod+SaaS+no-key errors.

5. **`internal/community/settings.go`** — `Settings` struct (pointer/Null
   fields = "unset"), `Repo.Settings(ctx, cid)` (lazy: missing row ⇒ zero
   Settings), `Repo.SaveSettings(ctx, cid, Settings)` (UPSERT), secrets via the
   `secretbox` (held by `Repo`). 

6. **`internal/community/resolve.go`** — the ONE resolution rule. Functions:
   `EffectiveAIEnabled`, `ResolveRAG`, `ResolveTranslate`, `ResolveStorage`,
   `JoinPolicy`, each `(Settings, config.Config) → effective`. `SAAS=false` ⇒
   return env directly. Kill-switch: env `*_ENABLED=false` ⇒ off regardless.
   verify: table test for the three resolution cases + self-host short-circuit.

7. **CLI** (`cmd/cli/main.go`): accept `role <email> owner`; add `owner <slug>
   <email>` to set/transfer ownership. verify: build + manual.

Commit boundaries: (a) migration+role, (b) secretbox+config, (c) settings+resolver+CLI.

---

## Phase 1 — Join policy (open vs request-approval)

Depends on Phase 0 (settings + resolver + owner gate).

1. `JoinPolicy(settings, cfg) → "open"|"request"`. Default `request`
   (preserves today's approve-queue behaviour); self-host ignores unless set.
2. Wire the **join** path: where a membership is created on join/register
   (`auth.Service.Register`/`activateAndJoin`, `service.go:203/249/347/488`,
   and the public-community join handler), stamp `approved_at = now` when policy
   is `open`, else `NULL` (today's behaviour). Cross-check the existing
   `OpenRegistrationAutoApprove` env so they compose (community policy wins in
   SaaS; env is the self-host default).
3. UI: owner/admin toggle on community admin (`/c/{slug}/admin`), a 2-state pill
   (`open` / `request`) — stable-id morph (§4.7).
4. verify: `open` → join auto-approved (no `/pending`); `request` → pending;
   service test.

---

## Phase 2 — Storage: Blobstore interface + S3 + per-community migration

Depends on Phase 0 (settings, secretbox, store_key column).

1. **`internal/uploads/blob.go`** — `Blobstore` interface (Put/Open/Remove/
   Exists). `diskBlobs{dir}` = today's `os` logic extracted from
   `uploads.go:169-253/281-405`. `s3Blobs{client,bucket,prefix}` via `minio-go`
   (S3-compatible: AWS/MinIO/R2).
2. **Refactor `uploads.Store`**: hold a default `Blobstore` + `blobstoreFor(ctx,
   cid) Blobstore` resolver (community S3 if migrated else default). `Save`/
   `SaveAttachment`/`SaveDataURL` write via `Put` and stamp `uploads.store_key`;
   `Get`+the handler stream via `Open`; `Delete` via `Remove` (keep the
   shared-file refcount check). Key scheme `<communityID>/<sha[:2]>/<sha><ext>`.
   Signing/HMAC/stable-URL (`uploads.go:298-373`) untouched.
3. Handler (`internal/uploads/handler.go`): stream `Blobstore.Open` instead of
   `os.Open`/`http.ServeFile` (use `http.ServeContent` with the reader).
4. **`uploads.MigrateCommunity(ctx, cid, dst Blobstore)`** — list community
   uploads, copy platform→dst, stamp `store_key`, set
   `community_settings.storage_migrated_at`. Idempotent, background job, progress.
5. config/main.go: `STORAGE_BACKEND` (SaaS default s3) builds the default
   `Blobstore`; S3 env (`S3_ENDPOINT/REGION/BUCKET/ACCESS_KEY/SECRET_KEY`).
6. Owner Storage card: backend display + "Migrate to my S3" (BYO fields,
   secrets write-only) → kicks the migration job.
7. verify: disk+s3 satisfy one Put/Open/Remove/Exists contract test (MinIO or
   fake); signed URL streams identical bytes from either; migrate copies +
   stamps + pre-migration reads still resolve. Existing uploads tests stay green
   (disk default, key scheme back-compatible — keep reading legacy `''` store_key
   from `Dir`).

---

## Phase 3 — Per-community AI master switch + owner gate

Depends on Phase 0.

1. `ai_enabled` in settings; `EffectiveAIEnabled(settings, cfg)` = `cfg.AIEnabled`
   (kill switch) AND community `ai_enabled` (default on self-host) — feed it into
   the existing "AI on?" checks (`agent.Handler`, nav gate, dispatch).
2. Owner gate: in SaaS, the agent **feature** toggle is owner-only; agent content
   CRUD (`/c/{slug}/admin/ai`) stays admin. Owner AI card: master on/off.
3. verify: community with `ai_enabled=false` → agent routes/nav hidden even with
   agents present; `AI_ENABLED=false` overrides all.

---

## Phase 4 — Per-community Translate

Depends on Phase 0.

1. `ResolveTranslate(settings, cfg) → (enabled, baseURL, model)`. The
   `/translate` handler (`internal/chat` or `agent.Translate` caller) reads the
   **community's** resolved config instead of the global `cfg.Translate*`.
2. Owner Translate card: enable + model + host.
3. verify: two communities, different models → each `/translate` hits its own;
   self-host falls back to env.

---

## Phase 5 — Per-community RAG + Qdrant (the big one)

Depends on Phase 0. Largest piece; may span several commits.

1. **`internal/rag/qdrant.go`** — implement `rag.Store` against Qdrant
   (qdrant-go client or REST). **Per-community collection** `collectionFor(cid)`
   created on demand with `size = community.embed_dim`, `distance=Cosine`.
   `Upsert`/`Query` target the community collection; `DeleteByRef` filters by
   `(kind, ref_id)` within it; `DropCommunity` drops the collection; `DropAll`
   drops all forumchat collections.
2. **Per-community embedder resolver**: `rag.Service`/`Worker` resolve a
   community's `Ollama` embedder from its `embed_base_url/embed_model/embed_dim`
   (closure wired in main.go from `community.ResolveRAG`); cache by
   `(cid, model, dim)`, invalidated on owner save+reindex.
3. **`Worker.tick`** (`internal/rag/worker.go`): group the dequeued `embed_outbox`
   batch by `community_id`; process each group with that community's embedder +
   collection; per-community backoff on embedder error (one bad tenant doesn't
   stall others).
4. **Backend selection**: SaaS default `RAG_BACKEND=qdrant`; chromem stays for
   self-host (single embedder, single collection-by-metadata — unchanged path).
   `ReindexCommunity` drops the community's collection (qdrant) before re-enqueue.
5. Owner RAG card: enable + embed model + host + dim + qdrant url/key +
   "Reindex this community". Saving model/dim ⇒ drop+recreate collection at new
   size + re-enqueue (explicit, warns it rebuilds).
6. verify: two communities, different `embed_dim` → two collections sized right;
   A's query never returns B's chunks; `DropCommunity` removes only A's; model
   change → reindex recreates at new dim. Qdrant-down → search degrades to empty,
   no 500. chromem path tests stay green.

---

## Phase 6 — Owner Settings shell + nav (interleaved, lands incrementally)

- `/c/{slug}/admin/settings` owner-gated route group (mounted only `SAAS=true`).
- Owner-ness in `web/templ` via a ctx-key leaf-package accessor (like
  `AdminAnyCtxKey`/super-admin §4.13); "⚙ Settings" tab shown to owners only.
- Each phase above contributes its card (AI/RAG/Translate/Storage/Join) into this
  shell as a stable-id morph fragment.

---

## Progress log

- 2606230830 — spec written + committed; plan written. Starting Phase 0.
- 2606230905 — **Phase 0 DONE** (owner role + 00055 migration; secretbox + SaaS
  boot rules; community Settings repo + resolver). **Phase 1 DONE** (join policy
  enforced in explore). **Phase 4 DONE** (translate resolved per-community).
  **Phase 3 DONE** (AI master switch: LoadCommunity stamps EffectiveAIEnabled,
  nav/admin-link gated, agent routes 404 when off). **Phase 6 (partial)**: owner
  Settings page `/c/{slug}/settings` (SaaS, owner-gated) with AI / join-policy /
  translate cards — makes Phases 1/3/4 operable. Full suite green (26 pkgs);
  SaaS boot smoke confirms route mounting + owner gate.
  - **REMAINING (largest, own follow-ups):**
    - **Phase 2 — Storage Blobstore + S3** (+per-community own-bucket migration).
      Needs a new dep (minio-go); `uploads.Store` blob backend extraction +
      `store_key` routing (column already added in 00055).
    - **Phase 5 — RAG + Qdrant per-community** (`internal/rag/qdrant.go`,
      per-community embedder resolver, worker fan-out by community). Net-new
      vector backend; chromem stays for self-host. Resolver `ResolveRAG` +
      `community_settings` RAG columns already in place to drive it.
    - **Phase 6 remainder** — RAG + Storage cards on the owner Settings page once
      those backends land.
  - The resolver (`community/resolve.go`) is the reusable seam: each remaining
    backend wires a closure from `ResolveRAG`/`ResolveStorage`, exactly like the
    translate closure in main.go.
- 2606231000 — **Audit pass + Phase 2 + Phase 5 SHIPPED.**
  - Security audit of the role refactor found 2 regressions (HIGH): migration
    00055 promotes admin→owner, but `AdminCommunityIDs`/`OldestCommunityAdminID`
    still hard-matched admin → a promoted owner lost /inbox + mailbox + /issues.
    Fixed (include owner) + regression test. Also: SSRF guard on owner-supplied
    URLs (`internal/netguard`, SaaS), and owner-bootstrap (new-community first
    member is now owner).
  - **Phase 2 (storage):** `Blobstore` interface + `diskBlobs` (refactor, disk
    identical) + `s3Blobs` (minio-go) + `STORAGE_BACKEND` wiring (SaaS→s3, falls
    back to disk if no bucket). Contract test. Per-community own-bucket
    **migration** still TODO (store_key column ready).
  - **Phase 5 (RAG/Qdrant):** `QdrantStore` REST impl, per-community collections,
    dynamic vector dim; `Service.EmbedderFor` per-community embedder; qdrant is
    the SaaS default backend; owner RAG card + reindex. chromem unchanged.
  - All 28 pkgs green; SaaS boot smoke OK. Only deferred: per-community S3
    own-bucket byte-migration + Storage card.
