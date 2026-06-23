---
name: spec-2606231130-fix-selfhost-regression
status: in-progress
type: spec
tldr: Second-pass review of the SaaS work focused on "did non-SaaS break?". It did — migration 00055 relabels the self-host admin → owner, and ~6 templ gates hard-compare Role == "admin" so the owner loses the Admin nav link, chat moderation menu, Lobbies link, etc. Fix with web/templ role helpers (owner ≥ admin). Plus one residual business-logic gap (reindex on Qdrant URL change). Verify both modes.
---

# Fix — self-host regression from the owner relabel + residuals

## Context

The SaaS work added an `owner` role and migration 00055 **promotes the
earliest admin per community → owner — in BOTH modes** (migrations can't see the
runtime `SAAS` flag). The Go gates were fixed to `Role.AtLeast(RoleAdmin)`
(owner ≥ admin), but the **templ UI** still hard-compares the role *string*
`== "admin"`, which `owner` fails. So every existing **self-host** deployment,
on upgrade, silently downgrades its admin's UI.

## Critical — self-host UI regression (breaks on upgrade)

`owner` fails these `Role == "admin"` / `"moderator"` templ gates → the promoted
owner loses:

| Gate | File:line | Lost affordance |
|---|---|---|
| Admin nav link | `web/templ/layout.templ:631` | **can't reach /admin from nav** |
| Lobbies nav link | `web/templ/layout.templ:626` | Lobbies hidden |
| Chat roster moderation menu | `web/templ/chat.templ:200` | ban / role actions hidden |
| Agent setup banner (admin view) | `web/templ/agent.templ:119` | wrong banner |
| Projects admin/mod affordance | `web/templ/projects.templ:636` | hidden |
| Roster role badge | `web/templ/roster.templ:84` | owner shows **no badge** |
| Super-admin role switcher | `web/templ/superadmin.templ:119,121` | owner not offered / mis-displayed |

**Fix:** add leaf-package helpers in `web/templ`:
- `RoleIsAdmin(role string) bool` → `role == "admin" || role == "owner"`
- `RoleIsMod(role string) bool` → `role == "moderator" || RoleIsAdmin(role)`

Replace the power gates with these; add an `owner` case to the roster badge and
the super-admin role switcher. (`layout.templ:608` `v.Role == "owner"` for the
Settings link stays exact — correct.)

This fixes self-host (owner == former admin, full UI restored) AND SaaS (real
owners need these affordances too). It is the consistent fix; the alternative
(don't promote in self-host) is impossible at migration time.

## Residual business-logic (second pass)

- **BL1 (MED):** the audit-B auto-reindex fires on embed model/dim change but NOT
  on **Qdrant URL/collection** change. Switching a community's Qdrant cluster
  (BYO) leaves the new cluster empty until content is next written. Extend the
  change-detection trigger in `admin.PostSettings` to include QdrantURL/Collection.

## Non-SaaS no-breaking-change audit (verify, don't assume)

Confirmed env-only (self-host unchanged) — re-verify by boot + tests:
- `config`: SaaS reshaping gated on `cfg.SAAS`; `RAG_BACKEND`/`STORAGE_BACKEND`
  defaults `""` resolved via `Effective*` (only readers); no direct reads elsewhere.
- `uploads`: disk Blobstore default; Save/Serve/Delete byte-identical (tests pass).
- `LoadCommunity`: self-host stamps `cfg.AIEnabled` with no extra DB read; nav
  gate equals the old `AIEnabled`.
- `community.resolve`: every resolver short-circuits to env when `!SAAS`.
- `explore` join: self-host `JoinPolicy` always `"request"` (unchanged pending flow).
- migration 00055 promotion is the ONLY data change in self-host — neutralised by
  the templ fixes above.

## Verification

- `go test ./...` green (shared tests).
- Boot `SAAS=false` (default) — app starts; smoke `/`, `/login`.
- Reason-check: with the helpers, an `owner` Viewer renders the Admin link +
  moderation menu (same set the old `admin` did).
- Boot `SAAS=true` — owner Settings still gated; no regression.

## Progress
- 2606231130 — spec written; implementing.
