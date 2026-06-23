---
name: audit-2606231100-saas-security-business-logic
status: in-progress
type: audit
tldr: Adversarial security + business-logic audit of the SaaS tenant-config work (Phases 0–6), cross-checked against mempalace memory. Memory accurately documents the *design*; this audit surfaces 3 real gaps the memory does NOT claim are handled (storage-flip-before-verify, RAG model-change-without-reindex, community-delete leaves the Qdrant collection) plus low-severity items. Fixes implemented one by one below.
---

# Audit — SaaS security & business logic

Scope: everything under `spec - saas-tenant-config` (owner role, secretbox,
settings/resolver, join policy, storage Blobstore+S3+migration, AI switch,
translate, RAG/Qdrant, owner Settings UI). Cross-checked against mempalace
`wing=forumchat` rooms `decisions`/`security` + `wing_forumchat` diary.

**Memory vs code:** the memory drawers describe the *design* faithfully and
match the code. They do **not** mention the gaps below — those are genuine
omissions, not contradictions. Memory will be updated after the fixes.

## Findings

### A — HIGH (business logic / availability): storage backend flips before the bucket is verified
`admin.PostMigrateStorage` calls `SaveSettings(StorageBackend=s3, …)` **before**
confirming the bucket is reachable. `NewS3Blobstore` is lazy (no connection), so
a wrong endpoint/bucket/credentials still saves successfully. Once saved,
`ResolveStorage` returns `OwnBucket=true`, so `Store.writeStoreFor` routes **new
uploads** to the broken bucket → every subsequent upload in that community fails,
and the migration goroutine errors out after the flip. The community is left in a
broken write state by a single bad form submit.
- **Fix:** build the destination store from the *merged* (submitted⊕stored)
  config, **probe connectivity** (a `Blobstore.Exists` round-trip = real
  `StatObject`/HTTP) and only `SaveSettings` + start the migration when the probe
  succeeds. On probe failure, render an error and change nothing.

### B — MED (business logic / silent breakage): RAG model/dim change without a reindex stalls the worker
The owner RAG card lets an owner change the embedding model or vector size and
save. The Qdrant collection `forumchat_<id>` was created at the *old* dimension;
upserting a new-dim vector into it fails, and the worker's first-error backpressure
then stops draining — embedding silently stalls (errors only in logs). The card
*tells* the owner to reindex but doesn't enforce it.
- **Fix:** in `PostSettings`, when the resolved embed model or dim changes,
  auto-trigger `Reindexer.ReindexCommunity` (drop collection + re-enqueue) in the
  background so the collection is recreated at the new size. Also clamp a negative
  dim to 0 (→ env default).

### C — MED (resource leak / privacy): deleting a community leaves its Qdrant collection (and S3 objects)
`superadmin.PostDeleteCommunity` → `community.Repo.Delete` cascades the DB rows
but never drops the per-community Qdrant collection. The `superadmin.Reindexer`
interface only exposes `ReindexAll`. So a deleted SaaS tenant's vectors persist in
Qdrant indefinitely — a storage leak and a privacy problem (embedded content of a
"deleted" community survives). (S3 object cleanup is the same shape but pre-exists
my work for disk files; vectors are new with Phase 5.)
- **Fix:** add `DropCommunity(ctx, communityID)` to `rag.Service` + the superadmin
  `Reindexer` interface; call it on community delete (best-effort, logged).

### D — LOW (availability): secretbox decrypt failure 500s the settings/join paths
If `SECRETS_KEY` is rotated, `secretbox.Open` fails and `community.Repo.Settings`
returns an error → `GetSettings`/`PostSettings`/explore-join 500. Documented as
out-of-scope (rotation orphans ciphertext), but the hard 500 is poor UX.
- **Fix (small):** `Settings` tolerates a per-field decrypt failure — log it,
  return the field empty — so the page still renders and the owner can re-enter
  the secret. Booleans/URLs (non-secret) are unaffected.

### E — LOW (perf, not security): `DeleteByRef` scans every `forumchat_*` collection
The community-less outbox delete fans out one HTTP delete per collection. Correct
(ref_id is a globally-unique UUID, no cross-tenant match) but O(collections) per
delete. Acceptable at current scale; documented. Proper fix would add
`community_id` to `embed_outbox` (migration + trigger rewrite) — deferred.

## Fix order
1. **A** (highest impact, smallest blast radius).
2. **C** (data hygiene, isolated).
3. **B** (worker correctness).
4. **D** (robustness).
5. E — documented only.

## Progress
- 2606231100 — audit written; starting fixes.
