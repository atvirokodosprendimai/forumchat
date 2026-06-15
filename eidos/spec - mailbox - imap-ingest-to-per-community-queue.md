---
name: spec-mailbox-imap-ingest
status: draft
type: spec
tldr: Single read-only IMAP mailbox ingested across all folders, per-community From: filters routing matched messages into a sorting inbox where attachments can be promoted to project docs and emails can auto-create editable issues.
---

# Mailbox — IMAP ingest with per-community filters

One mailbox feeds the whole forumchat instance. The poll worker walks every IMAP folder, remembers each per-folder UID cursor, and applies per-community From-address filters to decide which messages get ingested. Attachments and HTML bodies are NOT downloaded at poll time — only metadata is indexed. The user later clicks "move to project" on a queued attachment, which lazily fetches that MIME part and turns it into a `project_attachments` row pointing at an `uploads` blob. A filter can also opt into "auto-issue", which downloads the text/plain body, converts to markdown, and creates an editable `project_issues` row.

## Target

People send work into communities by email more often than they admit. Today the round-trip is: open mail client → save attachments to disk → re-upload them into the project docs section. The forum already has projects, issues, and docs — the gap is the silent ingestion layer that captures inbound email artifacts without manual download/re-upload.

The spec carves out one IMAP-side concern (a single shared mailbox, read-only, lazy fetch) and one forumchat-side concern (per-community routing into the existing projects/issues/docs surface) and bolts them together with a sorting queue UI.

The IMAP side is deliberately READ-ONLY: no \Seen flag mutation, no MOVE, no EXPUNGE. Mail clients keep working untouched.

## Behaviour

### Feature flag

- New env `MAILBOX_ENABLED` (bool, default `false`). Read once at boot via `internal/config`.
- When `false`: poll worker not started, queue route not mounted, sidebar item hidden, admin filter page hidden. Migration still runs so toggling on later requires no schema work.
- When `true`: poll worker spawns one goroutine; queue + admin pages mount under each community route.

### Poll loop — `internal/mailbox.PollWorker`

- Every `MAILBOX_POLL_INTERVAL` (default `2m`) the worker:
  1. Dials IMAP via `MAILBOX_TLS` mode (`tls` | `starttls` | `none`), authenticates with `MAILBOX_USER` / `MAILBOX_PASS`.
  2. `LIST "" "*"` to enumerate every folder.
  3. For each folder: `EXAMINE` (read-only select), compare `UIDVALIDITY` against `mailbox_folder.uidvalidity`. If mismatched → reset `last_uid` to 0 (full re-scan) and persist the new UIDVALIDITY.
  4. `UID FETCH <last_uid+1>:* (UID FLAGS ENVELOPE INTERNALDATE BODYSTRUCTURE)` — never `BODY[]` at poll time.
  5. For each message: lowercase the `From:` address, query `community_mail_filter` for a match.
     - No match → skip; do NOT insert.
     - Match → `INSERT OR IGNORE INTO email_ingest` (guarded by `UNIQUE(folder_id, uid, uidvalidity)`).
     - For each BODYSTRUCTURE attachment part → `INSERT INTO email_ingest_attachment` with metadata only (`filename`, `mime`, `size_bytes`, `mime_part_id`).
     - If matched filter has `to_issue=true` → fetch `BODY.PEEK[<text-part-id>]` only (not the whole message), html→text convert, call `projects.Service.CreateIssue` with the `MAILBOX_SYSTEM_USER_ID` as author. Link via `email_ingest_issue`.
  6. Update `mailbox_folder.last_uid` and `last_seen_at`. LOGOUT.
- A single bad folder does not abort the whole cycle; per-folder errors are logged + the next folder continues.
- The worker is single-instance; if NATS is added later we re-use the lock pattern, but the v1 deploy is one binary.
- Reads `BodySection{Peek:true}` everywhere. `\Seen` is never mutated; user's mail client view is undisturbed.

### Filter matching — `community_mail_filter`

Two filter kinds, both case-insensitive:

- `kind='domain'` — pattern stored as `@example.com` (the literal `@` is part of the pattern). Matches any sender ending in this string.
- `kind='address'` — pattern stored as `alice@example.com`. Exact equality.

Matching algorithm: extract `addr := lower(parseFromAddr(env.From))`; query for `address=addr OR domain='@'+split(addr,'@')[1]` and pick the **first match by precedence (`address` beats `domain`)**.

A community can have many filters. Filters across communities can overlap — the precedence rule is what disambiguates which community owns the resulting ingest row.

### Sorting queue — `/c/{slug}/inbox`

- Lists `email_ingest WHERE community_id=this AND status='queued'`, newest first.
- Each row: from, subject, received date, attachment count, "Create issue" / "Mark consumed" / "Discard" buttons.
- If the matched filter already auto-created an issue, a badge links to it.
- Each attachment renders one row inside the email row with: filename, size, mime, two dropdowns (project, category) + "Move" button.
- "Move" → backend lazily fetches the part, calls `uploads.Store.SaveAttachment`, inserts `project_attachments` row with the chosen `category`, stores the resulting `upload_id` on `email_ingest_attachment.upload_id`.
- When every attachment on an email is either moved or dismissed, the email row's status flips to `consumed` and falls out of the queue.
- "Discard" on the email row marks `status='discarded'` and hides it. Attachments stay indexed (for later forensic search) but cannot be materialized.

### Admin filter CRUD — `/c/{slug}/admin/mail-filters`

- Admin/mod-only page (RequireRole pattern from `internal/admin`).
- One table per kind (Domain filters / Address filters). New-row inline form. Delete button per row. No edit (delete + recreate).
- New filter form: pattern input + "Auto-create issue" checkbox + "Save".
- Saving normalises the pattern (lowercases, ensures `@` prefix for domain kind).

### Realtime morph

- Per-community in-memory `mailbox.Bus` with subject `community.<cid>.mailbox`. Fans out to:
  - Open `/c/{slug}/inbox` pages → re-render the queue list.
  - Open `/c/{slug}/projects/<id>` pages whose `project_attachments` got a new row → re-render the attachments fragment.
- Wire payload is the ingest_id or attachment_id; reader refetches via the read-model query (per AGENTS.md §6b "read model is a reusable pure function").

## Design

### Domain model — new pkg `internal/mailbox`

```
mailbox_account               (singleton; INSERT-on-first-boot)
  id, host, port, username, password, tls_mode,
  uid_validity_global INT,  -- unused; per-folder UIDVALIDITY lives below
  last_poll_at, last_error, created_at

mailbox_folder
  id, account_id (FK), name TEXT, uidvalidity INT, last_uid INT,
  enabled BOOL DEFAULT 1, last_seen_at, last_error
  UNIQUE(account_id, name)

community_mail_filter
  id, community_id (FK communities), kind TEXT CHECK(kind IN ('domain','address')),
  pattern TEXT, to_issue BOOL DEFAULT 0, created_by (FK users), created_at
  INDEX(kind, pattern)

email_ingest
  id, folder_id (FK), uid INT, uidvalidity INT, message_id TEXT,
  from_addr TEXT, from_name TEXT, subject TEXT, received_at,
  community_id (FK communities), status TEXT CHECK(status IN ('queued','consumed','discarded'))
    DEFAULT 'queued',
  matched_filter_id (FK community_mail_filter), created_at
  UNIQUE(folder_id, uid, uidvalidity)
  INDEX(community_id, status, received_at DESC)

email_ingest_attachment
  id, ingest_id (FK), filename TEXT, mime TEXT, size_bytes INT,
  mime_part_id TEXT,                -- e.g. "2", "2.1" — used in BODY.PEEK[2.1]
  upload_id (FK uploads, NULL until materialized),
  moved_to_project_id (FK projects, NULL),
  moved_category TEXT, moved_at, created_at
  INDEX(ingest_id)

email_ingest_issue
  ingest_id (PK, FK email_ingest),
  issue_id (FK project_issues),
  created_at
```

### Read-only enforcement

- Single file `internal/mailbox/imap.go` houses every IMAP call. CI grep gate: `grep -rE 'Store|Expunge|\.Move|\.Copy' internal/mailbox/` must return zero hits.
- Every `Select` uses `&imapclient.SelectOptions{ReadOnly: true}`.
- Every `Fetch` uses `BodySection{Peek: true}`.
- The mailbox user account on the server side SHOULD also be a read-only role if the IMAP server supports per-user ACLs.

### CQRS — read model is a pure function

Per AGENTS.md §6b:

```go
// internal/mailbox/repo.go — same function used by GET /inbox and SSE re-render.
func (r *Repo) QueueForCommunity(ctx, communityID string, limit int) ([]QueuedEmailView, error)
```

Write side only emits `Bus.Broadcast(communityID)` + NATS publish; payload is the community id. Subscribers refetch via the read model. No HTML on the wire.

### Lazy fetch — connection pool

- The poll worker holds one IMAP client open across folder iterations within a cycle, then closes.
- The "move attachment" handler opens a short-lived client on demand. No pool in v1 — IMAP login overhead is ~50–200ms, acceptable for an interactive click.
- If multiple attachments are moved in one minute → revisit pooling.

### HTML→text for auto-issue

- Use `github.com/jaytaylor/html2text` (no CGO, mature, MIT). Wraps lines at 80, preserves links as `[text](url)`.
- Issue body fields:
  - `body_md` = result of html2text (markdown-escape `*` and `_` chars first).
  - `body_html` = `render.RenderMarkdown(body_md)` — goes through existing pipeline so styling matches user-typed issues.
- Hard cap: 65,536 chars on `body_md` (truncate with `... [truncated]` if exceeded).

### Config additions

```go
MailboxEnabled        bool          `env:"MAILBOX_ENABLED" envDefault:"false"`
MailboxHost           string        `env:"MAILBOX_HOST"`
MailboxPort           int           `env:"MAILBOX_PORT" envDefault:"993"`
MailboxUser           string        `env:"MAILBOX_USER"`
MailboxPass           string        `env:"MAILBOX_PASS"`
MailboxTLS            string        `env:"MAILBOX_TLS" envDefault:"tls"`        // tls | starttls | none
MailboxPollInterval   time.Duration `env:"MAILBOX_POLL_INTERVAL" envDefault:"2m"`
MailboxAttachmentMax  int64         `env:"MAILBOX_ATTACHMENT_MAX" envDefault:"26214400"`  // 25 MiB
MailboxSystemUserID   string        `env:"MAILBOX_SYSTEM_USER_ID"`              // system author for auto-issues
```

Password env is plaintext in v1 (matches forumchat's existing pattern for `SESSION_KEY`, `UPLOADS_SIGN_KEY`, `SMTP_PASS`).

## Verification

- **Read-only**: spin up `dovecot` or `greenmail`, run the poller, then connect to the same mailbox via Thunderbird — every test message must still be unread (`\Seen` not set).
- **Idempotent**: stop the worker mid-cycle, restart — no duplicate rows; existing UIDs re-fetched but UNIQUE constraint absorbs the second insert; the `FindByUID` guard prevents side-effect re-runs (issue duplicates).
- **UIDVALIDITY rotation**: change the IMAP backing store's UIDVALIDITY (greenmail supports this), confirm the worker rescans from UID 0 in that folder without touching others.
- **Per-folder cursors**: drop a message in INBOX and another in `Archive`; confirm both are picked up and the cursors advance independently.
- **Filter precedence**: register `*@acme.com` → community A AND `alice@acme.com` → community B. Send from `alice@acme.com` → row appears in community B's queue. Send from `bob@acme.com` → community A.
- **Lazy fetch**: confirm `BODY[]` is NEVER sent during poll (capture IMAP traffic via `socat`). Confirm "Move attachment" issues exactly one `BODY.PEEK[<part>]` fetch.
- **Auto-issue**: filter with `to_issue=true`, send HTML-only email, confirm issue body has plaintext-converted body (no `<div>` literals), confirm issue is editable via existing edit handler.
- **Discard**: marking an email discarded must remove it from queue but keep its attachments indexed (forensic search future).

## Friction

- IMAP libraries are CGO-free, but `go-message/charset` adds ~2 MiB to the binary. Acceptable for this feature.
- A single shared mailbox is a security choice: every community admin can theoretically craft a filter that catches mail intended for another community. Filter precedence (exact > domain) mitigates accidental collisions; intentional poaching is a trust issue we accept.
- The first poll after enabling on a busy mailbox can be slow — full UID scan across every folder. Status spinner / progress log in admin page would help; out of scope for v1.
- IMAP IDLE could cut latency from 2 min → seconds but requires long-lived connections, reconnect logic, NOOP keepalives. Punt to v2.
- No support for OAuth / Gmail / Microsoft 365 modern auth in v1 — plain LOGIN over TLS only. Many providers require app-passwords; we document the setup, not solve it.

## Interactions

- Depends on [[spec - projects - per-community-collaborative-projects]] for the `project_attachments` row, `projects.Service`, and the docs UI.
- Depends on [[spec - project-issues - per-project-issues-with-guest-share-links]] for issue creation API and edit flow.
- Affects `internal/uploads` — relies on `Store.SaveAttachment` accepting arbitrary MIME (already true; not limited to images like the chat-paste path).
- Affects [[spec - forumchat - community web app with realtime chat and forum threads]] only by adding a new sidebar nav entry per community when `MAILBOX_ENABLED=true`.

## Mapping

> [[internal/mailbox/imap.go]]
> [[internal/mailbox/poll.go]]
> [[internal/mailbox/repo.go]]
> [[internal/mailbox/filter.go]]
> [[internal/mailbox/handler.go]]
> [[internal/storage/sqlite/migrations/00015_mailbox.sql]]
> [[web/templ/inbox.templ]]
> [[web/templ/mailfilters.templ]]
> [[cmd/app/main.go]]

## Future

- {[!] Phase 1 — schema + repo + poll worker shell logging UIDs across folders, no filter match.}
- {[!] Phase 2 — filter table + matching + `email_ingest` row insert.}
- {[!] Phase 3 — attachment metadata indexing from BODYSTRUCTURE.}
- {[!] Phase 4 — per-community sorting queue UI.}
- {[!] Phase 5 — lazy attachment materialize → project doc.}
- {[!] Phase 6 — `to_issue=true` auto-create issue with HTML→text.}
- {[!] Phase 7 — admin filter CRUD UI.}
- {[?] IMAP IDLE for sub-second arrival latency.}
- {[?] OAuth (Google / Microsoft) instead of plain LOGIN.}
- {[?] Per-attachment search across discarded emails — forensic recovery.}
- {[?] Filter-on-subject and filter-on-recipient (To/Cc) headers in addition to From.}

## Notes

### Open decisions still on the user

- Multi-community match — current spec says **first match wins, exact > domain**. If the user wants fan-out (queue the same email into both communities), this changes the schema (`email_ingest.community_id` becomes a many-to-many).
- Poll interval default 2m and IMAP IDLE deferred — confirm.
- Password storage as plaintext env — confirm; if AES-256-GCM at-rest is required, add `MAILBOX_KEY` (same shape as `UPLOADS_SIGN_KEY`).
- Oversize attachment behaviour — spec says "index metadata, refuse materialize above `MAILBOX_ATTACHMENT_MAX`". Alternative: skip metadata too.
- Auto-issue body cap 64 KB — confirm.

### Anti-enumeration

The queue UI is admin/mod-only per community (matches `internal/admin` pattern). Regular members do not see the inbox at all. This avoids leaking which outside senders are routed where.

### System user

`MAILBOX_SYSTEM_USER_ID` must be a real `users` row (auto-created at boot if missing, like the bootstrap community?). The auto-issue creator is this system user. Open: invent a "system" identity that's not a regular user — clearer audit trail. Defer to refine.
