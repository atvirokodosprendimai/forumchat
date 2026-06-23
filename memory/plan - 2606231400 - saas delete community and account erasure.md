---
status: in-progress
created: 2026-06-23
---

# Plan — SaaS: delete community (ALL data) + self-serve account erasure

## Decisions (user-confirmed, 2026-06-23)

1. **Account erasure = anonymize PII + scrub content** (NOT a hard `users` row
   delete — FK RESTRICT on projects/issues/lobbies/mailbox/time_budgets would
   either fail or nuke shared community data). Keep the row, scrub it to a
   tombstone; shared artifacts the user authored survive as "deleted user".
2. **Community delete = owner self-serve (Danger Zone in `/settings`, SAAS) +
   keep super-admin**, and BOTH also wipe upload blobs (disk/S3).
3. **Sole-owner block**: refuse account erasure while the user is the only owner
   of a community that has other members. Solo-owned communities (only member =
   the user) are deleted as part of erasure.
4. **2-step confirm**: current password → email a one-time deletion link →
   clicking lands on a final confirm page → confirming runs the irreversible
   erasure + signs out.

## Code reality (cited)

- `superadmin.PostDeleteCommunity` `internal/superadmin/handler.go:323` →
  `community.Repo.Delete` (`internal/community/community.go:225`, FK cascade) +
  `RAG.DropCommunity`. **Gap:** upload blobs are never removed.
- `provision.Service` `internal/provision/provision.go:23` is the community
  create→seed seam (has `Communities`+`Auth`); already wired into both
  `adminHandler.Provision` (`main.go:427`) and `superHandler.Provision`
  (`main.go:1914`). → extend it into the community-lifecycle seam with `Delete`.
- `uploads.Store.Delete` `internal/uploads/uploads.go:456` already dedup-aware
  (removes blob only when last row for the rel_path). Reuse it for bulk purge.
- RAG self-purges: `chat_messages`/`threads`/`posts` `AFTER DELETE` triggers
  enqueue `embed_outbox` `delete` (migration 00039:49,73,97). Hard-deleting the
  user's content drops their vectors via the worker. `chat_messages.author_id`
  is `ON DELETE SET NULL` (00001:57) → must be deleted explicitly.
- `auth.Service.IssueMagicLink`/`ConsumeMagicLink` `internal/auth/service.go:285`
  + `verification_tokens.purpose` → reuse for purpose `account_delete`.
- `CheckPassword` `internal/auth/password.go:18`; `oauthSentinelHash` `oauth.go:16`.
- Owner Settings: `admin.GetSettings/PostSettings` `internal/admin/settings.go`;
  routes mounted SAAS+owner `main.go:1712`.

## Steps

### A. Community delete with ALL data (shared seam)
1. `uploads.Store.DeleteByCommunity(ctx,cid)` + `DeleteByOwner(ctx,uid)` —
   `SELECT id WHERE col=?` loop `s.Delete` (reuses dedup + blob removal).
2. `provision.Service`: add `Uploads`, `Vectors VectorDropper`, `Log`; add
   `Delete(ctx,cid)` = purge blobs → `Communities.Delete` (cascade) → drop
   vectors. main.go: set `provSvc.Uploads/Vectors/Log`.
3. `superadmin.PostDeleteCommunity` → call `h.Provision.Delete` (drop inline
   community+RAG delete).
4. Owner Danger Zone: `admin.PostDeleteCommunity` (`/c/{slug}/settings/delete`,
   type-slug-to-confirm) + templ card; mount in SAAS owner group.

### B. Account erasure (anonymize + scrub), 2-step
5. `auth.Repo`: `SoleOwnerBlockers`, `SoloOwnedCommunityIDs`,
   `PurgeUserData(tx,uid)` (memberships + content + auth/identity + personal),
   `AnonymizeUser(tx,uid,tombstone)`.
6. `auth.Service`: `IssueDeletionLink`, `CheckDeletable`, `DeleteAccount` — guard
   sole-owner → delete solo communities via `CommunityDeleter` iface → tx purge
   + anonymize → owned uploads via `UploadPurger` iface.
7. `auth.Handler`: `PostDeleteStart` (RequireAuth, password), `GetDeleteConfirm`
   + `PostDeleteConfirm` (public, token-gated). Routes + `GoodbyePage`.
8. templ: `DeleteAccountCard`, status fragment, confirm page, goodbye; signals in
   `InitialSignals`.
9. CLI `delete-account <email>` (ops + test seam).
10. Tests: `auth/delete_account_test.go`, uploads bulk delete, provision delete.

## Verification
`make gen && make build && make test` per commit. Commit+push per step.
