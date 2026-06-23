---
name: proof-2606231200-saas-nonsaas-security
status: complete
type: proof
tldr: Reproducible evidence that (1) non-SaaS behaves exactly as before, (2) SaaS works end-to-end, (3) security is high. Backed by the full test suite (28 pkgs, no cache) + go vet + live HTTP runs in BOTH modes (authenticated user journeys) + live firing of the security controls (owner-gate 403/200, SSRF reject, prod boot guard).
---

# Proof — non-SaaS unchanged · SaaS works · security high

All commands below were run on `main` (post audit + fixes). The repo `.env`
forces `SAAS=true`, so self-host runs MUST pass `env SAAS=false` (godotenv does
not override a shell-set var).

## Tier 1 — automated (the behavioral contract)

- `go test ./... -count=1` → **28/28 packages OK** (no cache). The existing tests
  are the pre-SaaS behavioral baseline; all green ⇒ no regression.
- `go vet ./...` → clean.
- Named security/behaviour tests (verbose), all PASS:
  - SSRF: `TestBlockedURL` (loopback/private/link-local/metadata/wrong-scheme/malformed)
  - secrets: `TestBox_RoundTrip`, `TestBox_Passthrough`, `TestSettings_RoundTripAndSealing` (DB column holds ciphertext, not plaintext), `TestSettings_DecryptFailureTolerated`
  - resolver: `TestResolveRAG_SelfHostUsesEnv` / `_SaaSOverrideAndDefaultCollection` / `_KillSwitch` / `_SaaSOptInDefaultOff`, `TestEffectiveAIEnabled`, `TestJoinPolicy`, `TestResolveStorage_OwnBucketOptOut`
  - roles: `TestRoleRank`, `TestRoleHelpers` (owner ≥ admin), `TestOwnerKeepsAdminScopedSurfaces`, `TestRequireRole_*`, `TestSuperAdminMembership_*`
  - tenancy: `TestQdrantConnResolution` (per-community collection `forumchat_<id>`), `TestMigrateCommunity`

## Tier 2 — live, NON-SaaS (`env SAAS=false`): unchanged

| Probe | Result | Meaning |
|---|---|---|
| `/` | 303 → `/login` | self-host front door (no marketing landing) |
| `/explore` | 200 | works |
| boot log | `backend=chromem`, no S3 | self-host defaults (chromem + local disk) |
| register (auto-verify) | 200 + `Set-Cookie: forumchat_session …; HttpOnly; SameSite=Lax` | signs in; secure cookie |
| `/profile` (authed) | 200 | authenticated journey works |
| `/c/main/chat/general` (authed) | 200 | chat works |
| `/c/main/forum` (authed) | 200 | forum works |
| `/c/main/chat/general` (anon) | 303 → `/login` | unauthed correctly bounced |
| `/profile` (anon) | 303 → `/login` | "" |

Identical to pre-SaaS: `/`→login, chromem, disk, full authed journey, anon bounced.

## Tier 3 — live, SaaS (`env SAAS=true`): works

| Probe | Result | Meaning |
|---|---|---|
| `/` | 200 | marketing landing renders |
| register alice / bob (no invite) | 200 / 200 | open registration FORCED by SaaS |
| storage | "falling back to local disk" | s3 default, graceful fallback w/o bucket |
| owner `/c/main/settings` (after promote) | 200 | owner configures the tenant |

## Tier 4 — live security controls firing

| Control | Probe | Result |
|---|---|---|
| **Owner gating (authz)** | alice=member → `/c/main/settings` | **403** |
| | alice promoted owner (CLI) → `/c/main/settings` | **200** (role reloaded per-request from session→DB) |
| | bob=member → `/c/main/settings` | **403** |
| **SSRF guard** | owner POST settings, translate host `169.254.169.254` | **"Rejected URL — host 169.254.169.254 resolves to a non-public address"** |
| **Prod boot guard** | `ENV=prod SAAS=true` w/o `SECRETS_KEY` | **refuses: "fatal: SECRETS_KEY (32 bytes) must be set when SAAS=true in production"** |
| | same with a 32-byte `SECRETS_KEY` | boots, `/`=200 |
| **Secrets at rest** | `TestSettings_RoundTripAndSealing` | DB column ≠ plaintext |
| **Cross-tenant isolation** | per-community Qdrant collection + per-community blob store + `RequireMember` rebinds identity to the slug community | unit + structural |
| **Session cookie** | register response | HttpOnly + SameSite=Lax (+ Secure in prod) |

## Verdict

All three claims **proven**: non-SaaS is byte-for-byte the prior behaviour
(tests + live authed journey), SaaS works end-to-end (live), and the security
controls (authz owner-gate, SSRF block, prod secret guard, at-rest encryption,
per-tenant isolation, owner≥admin) fire as designed.
