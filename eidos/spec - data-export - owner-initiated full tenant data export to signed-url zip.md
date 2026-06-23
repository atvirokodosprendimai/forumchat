---
name: data-export
status: active
claim: an owner can request a background-built ZIP of ALL their community's data + media, downloadable via a 7-day signed URL, excluding platform-owned property (agent system prompts, RAG vectors)
---

# Data export — owner-initiated full tenant data export

## Intent

The inverse of community-delete + account-erasure (`CLAUDE.md` §5g). A SaaS
tenant owner can take **all of their community's data** with them: a background
job collects every business function into `folder/file.json` files plus the raw
media, packages a ZIP, and exposes it behind a **signed download URL valid 7
days**. After expiry the artifact is deleted and a new request is required.

GDPR data-portability for the tenant. Owner-only, per-community, **only this
community's data — never other tenants'**.

## What is exported

Organized by business function (one folder per domain, one JSON array per
table), plus a `media/` folder with the raw uploaded bytes:

- `community/` — community row, non-secret settings
- `members/` — memberships, member users (no password_hash), invites, bookmarks,
  personal todos, blocks, reports
- `chat/` — channels, messages, message↔attachment links, pastes
- `forum/` — threads, posts
- `agents/` — agent identities (name only), AI threads, AI messages
  (the `[{role, body}]` prompt/response turns)
- `projects/` — projects, comments, todos, attachments, guest invites, issues,
  issue comments/attachments, discussion threads/replies, time entries
- `lobbies/` — lobbies, lobby messages
- `rooms/` — rooms, room chat
- `webhooks/` — webhook definitions (no token/secret)
- `mailbox/` — mail filters, ingested email
- `manifest.json` — export metadata (community, generated_at, table→file map,
  exclusions note)

## What is NOT exported (platform property / safety)

- **Agent system prompts + API keys** (`ai_agents.system_prompt`,
  `api_key_enc`) — platform IP, per the customer's own instruction.
- **RAG vectors / embeddings** (`embed_outbox`, Qdrant/chromem) — platform
  property; the vectors are ours, not the tenant's.
- **Secrets, by rule** — any column named `password_hash`, ending `_enc`, or
  containing `secret` / `token` / `api_key` / `*_key` is redacted from EVERY
  table (webhook tokens, lobby guest tokens, Qdrant key, S3 creds…).
- **Transient / auth internals** — sessions, verification/signup tokens, push
  subscriptions, read-state, OAuth identities, debug logs, migration-artifact
  tables.
- **Cross-party private DMs** — `private_threads`/`private_messages` involve a
  second member; excluded so the export never leaks "others'" data.

## Lifecycle

1. Owner clicks **Generate export** (`/c/{slug}/settings`, Danger-Zone-adjacent
   card). One active export per community.
2. A `community_exports` row is created `pending`; the `ExportWorker` (a
   background queue, like `uploads.SweepWorker`) drains it: `building` → write
   ZIP to `<uploads>/exports/<id>.zip` → `ready` with a 32-byte capability token
   and `expires_at = ready_at + 7d`.
3. Download via `GET /c/{slug}/settings/export/{id}/download?token=…` — **public,
   token-gated** (the signed URL). High-entropy token = bearer capability, same
   pattern as the portal module's 7-day links.
4. A periodic sweep deletes ZIPs past `expires_at`, marks the row `expired`. A
   fresh request supersedes any prior artifact.

## Design (code seams)

- **One generic dumper + a declarative manifest** (DRY) — `SELECT * FROM <table>
  WHERE <scope>`, rows → `[]map[string]any`, secret columns dropped. Table and
  WHERE are internal constants (no user input → no injection, like
  `uploads.deleteWhere`). Indirect tables scope via subquery
  (`thread_id IN (SELECT id FROM threads WHERE community_id=?)`).
- **`internal/dataexport`** package: `manifest.go`, `repo.go` (table + dumper),
  `service.go` (Build), `worker.go`, `handler.go`. Imports `uploads` for media
  bytes; defines no domain imports → no cycle.
- **UX**: SSE-streamed status card (idiomatic datastar) — live `building → ready`
  without reload.

## Verification

- Scoping test: two communities, export A contains only A's rows.
- Redaction test: `password_hash`, `system_prompt`, webhook `secret`/`token`
  never appear in the ZIP.
- Lifecycle test: request → build → ready → token download works; expired token
  refused; expiry sweep removes the file.
