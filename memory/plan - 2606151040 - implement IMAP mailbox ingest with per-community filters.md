---
tldr: Build IMAP mailbox ingest in phases that each end with something visible. Phase 1 ships the empty global inbox page behind a flag; Phase 2 dials IMAP and logs UIDs across all folders without writing rows; subsequent phases add filter matching, attachment metadata, the browse UI with community filter pills + infinite scroll, lazy attachment materialisation into project docs, and opt-in auto-issue creation.
status: active
---

# Plan: Implement IMAP mailbox ingest with per-community filters

## Context

- Spec: [[spec - mailbox - imap-ingest-to-per-community-queue]] (commit `1741fd7` on `task/spec-mailbox-imap-ingest`)
- Adjacent specs (read-only patterns to mirror, not modify):
  - [[spec - projects - per-community-collaborative-projects]] — `project_attachments`, `projects.Service`, attachments docs UI
  - [[spec - project-issues - per-project-issues-with-guest-share-links]] — `Issue` creation API, edit grace, identity union (user vs guest)
  - [[spec - forumchat - community web app with realtime chat and forum threads]] — datastar + SSE patterns and `viewer(r)` flow
- Existing patterns to reuse:
  - `internal/push/digest.go` `DigestWorker.Start(ctx)` — ticker goroutine with graceful shutdown via `<-ctx.Done()` (see `cmd/app/main.go:312`)
  - `internal/projects/chat_digest.go` `ChatDigestWorker` — interval worker with per-community state cursor table; the closest existing analogue for the IMAP poll loop
  - `internal/uploads/uploads.go` `Store.SaveAttachment` — arbitrary MIME accepted, SHA-256 dedup; takes `(ctx, ownerID, communityID, mime, filename, io.Reader) → Upload`
  - `internal/community/middleware.go` `LoadCommunity` / `RequireMember` — for the per-community filter CRUD page; **NOT used by the global inbox** which lives off `/inbox`
  - `internal/auth/middleware.go` `RequireAdmin` (or RoleAtLeast pattern) — for global inbox + admin filter access gate
- Prior art (vvs project, via mempalace `drawer_memory_general_*` tier2_email.md):
  - go-imap/v2: UIDs per-folder not per-account → schema gets `(account_id, name, last_uid, uidvalidity)`
  - `EXAMINE` + `BODY.PEEK[]` + targeted `STORE -FLAGS \Seen` to undo accidental seen-mutation. We do NOT need the STORE step because we never trigger \Seen with PEEK.
  - go-imap/v2 `Envelope` lacks `References` — parse from raw body via `mail.CreateReader` when needed (probably not in v1 — we don't thread by References yet).
  - `go-message/charset` init() global hook auto-decodes UTF-8 → `io.ReadAll(part.Body)` directly. NEVER `DecodeReaderToUTF8` on top → double-decode garbles text.
  - Idempotent `processMessage`: guard with `FindByUID` BEFORE doing side effects. `INSERT OR IGNORE` alone lets push/notify/issue-create side-effects run twice.
- User clarifications (this session):
  - The mailbox is a **single global** account — there is ONE `/inbox` page (admin-only), not one per community. Community filters live as **pills/dropdown** on the global page.
  - "Filter per community" = a UI affordance on the global page. The DB row's `community_id` is still authoritative for routing.
  - No outbound — no compose, no reply, no MOVE. Strictly READ-ONLY ingest.
  - Browse experience: last 100 emails by default, infinite scroll to fetch more. Cursor-paginated by `(received_at DESC, id DESC)`.
- Watch out for:
  - `datastar.NewSSE` flush-before-cookie bug → use `commitSession` if any session mutation happens around a stream handler (unlikely here; the inbox is read-only).
  - chi compressor + SSE → use `render.NewSSE` (already wraps the prime call). Same as projects.
  - SQLite `MaxOpenConns=1` — IMAP poll worker holds no DB transactions across IMAP I/O. Poll batches reads + writes in tight blocks.
- Plan-shape decisions:
  - The spec's per-community queue UI is wrong relative to user intent. Phase 0 refines the spec to global `/inbox` + community filter pills + infinite scroll BEFORE any code lands so the rest of the plan implements the corrected design.
  - Phase order optimises **visible result early** — Phase 1 ends with the empty `/inbox` page reachable behind the flag. Phase 2 ends with poll-loop logs showing "I saw UIDs 42, 43 in INBOX, 11 in Sent" — no DB writes yet. Phase 3 starts persisting matched rows so the UI in Phase 5 has something to show.
  - The spec branch (`task/spec-mailbox-imap-ingest`) carries both spec and this plan. First implementation phase merges spec + plan + Phase 1 to main together so the spec ships alongside its first verifiable bit of code.

## Phases

### Phase 0 — Refine spec for global inbox shape — status: completed

Goal: the spec's UI section reflects user intent: **global `/inbox` page** (admin-only), community filter pills along the top, default 100-row list with infinite scroll cursor pagination, no per-community `/c/{slug}/inbox` route at all.

1. [x] `/eidos:refine [[spec - mailbox - imap-ingest-to-per-community-queue]]`
   - => Replaced "Sorting queue — `/c/{slug}/inbox`" with "Global inbox — `/inbox`"; added community filter pills, `$inbox_community` signal, `?community=<id>` URL reflection, "All" view.
   - => Replaced "newest first" with last-100 + `data-on:scrollend` → `/inbox/more` → `WithModeAppend()`. Cursor format documented as opaque base64-url of `received_at_unix_ms || ':' || id`.
   - => Added Anti-enumeration block in Behaviour: `/inbox` returns 404 for non-admins (not 401/403). Community pills only render viewer's admin communities.
   - => Resolved gate decision: admin-of-any-community page entry; row-level community scoping via `viewer.AdminCommunityIDs`.
2. [x] Added UX bullet for click-sender-attach popover with the same handler path as `PostCreateFilter`.
3. [x] Committed spec refine on `task/spec-mailbox-imap-ingest`.
   - => commit hash to be filled by the refine commit below

### Phase 1 — Feature flag, schema, empty global inbox page — status: completed

Goal: `MAILBOX_ENABLED=true` shows an "Inbox" link in the topbar (visible only to admins of any community) and `/inbox` renders an empty-state page. `false` hides everything. DB tables exist regardless so toggling on later requires no schema work.

1. [x] Migration `internal/storage/sqlite/migrations/00020_mailbox.sql` — `mailbox_account`, `mailbox_folder`, `community_mail_filter`, `email_ingest`, `email_ingest_attachment`, `email_ingest_issue` per spec §Domain model
   - => Chose `00020` because chain reached `00019_lobbies.sql` already.
   - => FK chain works: `email_ingest_issue.issue_id` → `project_issues(id)` already exists in `00014`.
2. [x] `internal/config/config.go` — added 8 env vars: `MAILBOX_ENABLED/HOST/PORT/USER/PASS/TLS`, `MAILBOX_POLL_INTERVAL`, `MAILBOX_ATTACHMENT_MAX`, `MAILBOX_SYSTEM_USER_ID`.
3. [x] `internal/mailbox/types.go` — `Account`, `Folder`, `Filter` (+`FilterKind` enum), `Ingest` (+`IngestStatus` enum), `Attachment`, `QueueCursor`, `QueueQuery`.
4. [x] `internal/mailbox/repo.go` — Phase 1 surface: `EnsureAccount` (insert-or-update singleton), `ListEnabledFolders`, `QueueForViewer` (cursor-paginated, scoped to viewer's admin community set, optional community pill, batched attachment fetch).
   - => Cursor format: opaque base64-url of `received_at_unix_ms || ':' || id`. Roundtrip + bad-input tests in `cursor_test.go`.
5. [x] `internal/mailbox/handler.go` — `Handler{Repo, AuthRepo, CommunityRepo, Log}` + `GetGlobalInbox`. Anti-enum: non-admin → `404 not found`. Wrong-community-pill → `404`.
6. [x] `web/templ/inbox.templ` — `InboxPage(d InboxPageData)` with community pills, empty-state, `#inbox-more` scrollend sentinel.
7. [x] `cmd/app/main.go` — wired `mailboxRepo`, `mailboxHandler`, `webtempl.MailboxEnabled = cfg.MailboxEnabled`, `/inbox` GET mounted only when flag true. EnsureAccount runs at boot when host+user envs are set.
8. [x] `web/templ/layout.templ` — `Viewer.IsAdminOfAnyCommunity` added; "Inbox" topbar entry inside `if MailboxEnabled && v.IsAdminOfAnyCommunity`.
9. [x] `internal/auth/repo.go` — `AdminCommunityIDs(ctx, userID) ([]string, error)` returns the admin/mod set, used by `GetGlobalInbox`.
10. [x] `internal/mailbox/cursor_test.go` — roundtrip + bad-input tests.

Verification (deferred to manual smoke once a `.env` has `MAILBOX_ENABLED=true`): admin viewer sees topbar link, `/inbox` returns 200 with empty state. Non-admin viewer sees no link and gets 404 on `/inbox`. Disabled flag — link absent, route 404.

### Phase 2 — IMAP poll loop shell, all folders, no DB writes — status: open

Goal: with valid `MAILBOX_*` env vars set, the poll worker dials IMAP every `MAILBOX_POLL_INTERVAL`, EXAMINEs every folder, logs `[mailbox] folder=INBOX uidvalidity=12345 last_uid=0 new_uids=[42,43]` and similar. No row writes. Lets us verify connectivity, read-only behaviour, and folder enumeration before adding any state.

1. [ ] `go get github.com/emersion/go-imap/v2 github.com/emersion/go-message`
2. [ ] `internal/mailbox/imap.go` — `imapClient` wrapper: Dial (with TLS mode switch), Login, ListFolders, ExamineFolder (read-only Select), FetchEnvelopes(uidStart..). Single file holds every IMAP method so the CI grep gate has one target.
3. [ ] `internal/mailbox/poll.go` — `PollWorker{Cfg, Repo, Log}` with `Start(ctx)` spawning a goroutine; `time.NewTicker(cfg.MailboxPollInterval)`; per cycle: Dial → ListFolders → for each folder Examine + FetchEnvelopes from `last_uid+1` (last_uid=0 first time) → log results → Close
4. [ ] `cmd/app/main.go` — when `MAILBOX_ENABLED=true` and host/user/pass set, construct and `.Start(workerCtx)` the worker. workerCtx is the same context as other digest workers
5. [ ] CI grep gate — add `make lint-mailbox` rule (or just a comment in the Makefile + a short shell-out in CI): `! grep -rE "Store\(|Expunge|\.Move\(|\.Copy\(|BodySection\{Peek: false" internal/mailbox/`

Verification: `MAILBOX_ENABLED=true` against a test IMAP (greenmail / docker `dovecot/dovecot`) → app logs show one Dial per interval, folder enumeration, UID fetches. Manually verify in Thunderbird (or `mbsync` on a sibling client) that test messages remain unread after several poll cycles.

### Phase 3 — Filter table + matching + email_ingest writes — status: open

Goal: emails matching any `community_mail_filter` get persisted; non-matches are silently skipped. Idempotent.

1. [ ] `internal/mailbox/filter.go` — `MatchFrom(ctx, repo, fromAddr string) (Filter, ok, error)` implementing precedence: exact address > wildcard domain
2. [ ] `internal/mailbox/repo.go` — `ListFilters(ctx) ([]Filter, error)` (cached in-memory + invalidated on filter mutate), `InsertIngest(ctx, params) (id string, isNew bool, err error)` with `INSERT OR IGNORE` returning rowcount; FindByUID guard surfaces `isNew=false` so side effects do not re-fire
3. [ ] `internal/mailbox/repo.go` — `UpsertFolder(ctx, accountID, name string, uidvalidity, lastUID uint32) error`. Called per cycle. Handles UIDVALIDITY rotation: if stored != observed → reset lastUID to 0 (full rescan next time) BEFORE this batch's writes
4. [ ] `internal/mailbox/poll.go` — extend cycle: for each fetched message, call `MatchFrom`. On hit, `InsertIngest`. Persist `last_uid = max(last_uid, msg.UID)` after the batch
5. [ ] Tests `internal/mailbox/filter_test.go` — precedence cases, lowercasing, malformed From: header, no-match path
6. [ ] Tests `internal/mailbox/repo_test.go` — UIDVALIDITY rotation, duplicate UID insert returns isNew=false, cursor advancement (use `t.TempDir()` SQLite)

Verification: insert an exact filter for `alice@acme.com` → community A and a domain filter for `*@acme.com` → community B. Send 3 emails (alice@, bob@, marketing@acme.com). DB has 3 `email_ingest` rows, alice in A, the other two in B. Re-run poll: no duplicates.

### Phase 4 — Attachment metadata from BODYSTRUCTURE — status: open

Goal: every matched email's attachments are indexed with metadata only (no bytes). The queue view can render filenames/sizes/mimes without ever having pulled the file body.

1. [ ] `internal/mailbox/imap.go` — extend FetchEnvelopes to also fetch BODYSTRUCTURE; helper `walkAttachmentParts(bs) []ParsedPart` returning `{MimePartID, Filename, MIME, SizeBytes}` from `Content-Disposition: attachment` or fallback heuristic (any non-text/* non-multipart leaf with a filename)
2. [ ] `internal/mailbox/repo.go` — `InsertAttachments(ctx, ingestID string, parts []ParsedPart) error` (batch INSERT inside tx so partial failure rolls back)
3. [ ] `internal/mailbox/poll.go` — after `InsertIngest` with `isNew=true`, call `InsertAttachments`. If `isNew=false`, skip — we already indexed.
4. [ ] Tests `internal/mailbox/imap_test.go` — feed a sample multi-part RFC 822 (helper file checked in to `testdata/`) through the parser, verify part IDs match BODYSTRUCTURE numbering (1, 2, 2.1 …)

Verification: send a real email with 2 attachments through the test mailbox. After one poll cycle: `email_ingest` has 1 row, `email_ingest_attachment` has 2 rows with matching filenames, sizes, mime types, mime_part_id values.

### Phase 5 — Global inbox queue UI with community filter pills + infinite scroll — status: open

Goal: `/inbox` renders last 100 ingest rows the admin can see, with community filter pills along the top. Scroll-end fetches the next 100.

1. [ ] `internal/mailbox/repo.go` — implement the `QueueForViewer` body declared in Phase 1: SELECT JOIN ingest + attachments aggregate (count), filter by `viewerAdminCommunityIDs` AND optional `communityFilter`, cursor-paginate `WHERE (received_at, id) < (?, ?) ORDER BY received_at DESC, id DESC LIMIT ?`
2. [ ] `internal/mailbox/handler.go` — `GetMore(w, r)` — parses `?cursor=` + `?community=`, returns SSE that `PatchElementTempl(InboxRows(views), WithSelector("#inbox-rows"), WithModeAppend())` and updates the `#inbox-more` sentinel cursor
3. [ ] `web/templ/inbox.templ` — `InboxPage` populated: pills loop, `#inbox-rows` container, each row a `<details>` with email summary in summary and attachment rows inside; `#inbox-more` carries `data-on:scrollend="@get('/inbox/more?cursor='+$next_cursor+'&community='+$inbox_community)"`
4. [ ] `web/static/app.css` — pill style + row hover + attachment grid (minimal — match projects shell)
5. [ ] `internal/mailbox/bus.go` — per-community Bus (map[communityID]map[chan]). Phase 3's poll loop calls `Bus.Broadcast(communityID)` after each batch finishes. `Handler.GetStream` subscribes to all communities the viewer is admin in, re-renders the first page on any signal. NATS subject `community.<cid>.mailbox`. See AGENTS.md §4.11 per-X Bus.
6. [ ] Inline "Attach sender → community" popover. Each inbox row's sender chip carries `data-on:click="$attach_addr='<from_addr>'; $attach_open=true"`. Popover (`<dialog>` morph-pattern from rooms) contains: read-only address, kind toggle (exact / `@domain.tld`), community `<select>` (admin-of communities only), "Save" → `@post('/inbox/attach-sender')`. Backend creates the filter via the same code path as Phase 8's `PostCreateFilter`. After save: close popover, `Bus.Broadcast(communityID)`, no full reload.

Verification: load `/inbox` with 250 rows in DB → see first 100 → scroll to bottom → 100 more append → scroll again → 50 more append + sentinel hides. Click a community pill → first 100 of that community only. Run poll worker, send a new email matching a filter → list morphs in within ~1s.

### Phase 6 — Lazy attachment materialise → project doc — status: open

Goal: from the inbox, an admin picks a project + category from per-attachment dropdowns and clicks "Move". The chosen attachment is fetched (BODY.PEEK[mime_part_id]), saved to uploads, and linked into `project_attachments`.

1. [ ] `internal/mailbox/handler.go` — `PostMoveAttachment` — reads signals `{attachment_id, project_id, category}`, opens IMAP client on demand, fetches the single part, streams into `uploads.Store.SaveAttachment(ctx, systemUserID, communityID, mime, filename, reader)`, inserts `project_attachments`, updates `email_ingest_attachment.upload_id + moved_to_project_id + moved_category + moved_at`
2. [ ] `internal/mailbox/service.go` — extract the orchestration into `Service.Materialise(ctx, attachmentID, projectID, category, mover Identity) (Attachment, error)` — keeps the handler thin and testable
3. [ ] `internal/mailbox/repo.go` — `AttachmentByID(ctx, id) (Attachment, Ingest, error)` returning both the attachment and its parent ingest (for community + IMAP folder + UID + mime_part_id). Single SELECT JOIN. `MarkConsumedIfAllMoved(ctx, ingestID) (bool, error)` — flips `email_ingest.status='consumed'` when no unresolved attachments remain
4. [ ] `web/templ/inbox.templ` — project + category `<select>` dropdowns per attachment row (project list is the viewer's allowed projects in that community), "Move" button
5. [ ] After-move SSE — re-render that ingest row (`PatchElementTempl(InboxRow(view), WithSelector("#ingest-<id>"))`) AND publish `Bus.Broadcast(communityID)` so other open inbox tabs morph
6. [ ] Cross-community guard — the chosen project must belong to the ingest row's `community_id`; otherwise 403. Tests `internal/mailbox/handler_test.go` cover this

Verification: queue has an email with 2 attachments. Pick project P + category "design" → click Move on attachment 1 → file lands in project P's docs section with category "design", attachment row in queue now shows "Moved → P", row stays in queue. Move attachment 2 → email row disappears from queue (status=consumed) and reappears under a future "Show consumed" toggle (deferred).

### Phase 7 — Filter `to_issue=true` → auto-create editable issue — status: open

Goal: filters can mark `to_issue=true`. When the poll loop matches such a filter, it fetches text/plain (or html→text-converted) body, creates a `project_issues` row in some default project per community, links via `email_ingest_issue`.

1. [ ] `go get github.com/jaytaylor/html2text`
2. [ ] `internal/mailbox/bodyparse.go` — `ExtractIssueBody(parsed ParsedMessage) string` — prefer `text/plain` part, fall back to html2text on `text/html`, markdown-escape `*` `_`, truncate to 64 KB with `... [truncated]`
3. [ ] Open decision in Notes / spec: which project gets the auto-issue per community? Three options on offer:
   - Per-community default project chosen at filter-create time (new column `community_mail_filter.default_project_id`)
   - First active project in community by `updated_at DESC` — simplest but unpredictable
   - A new "Inbox" virtual project auto-created per community on first auto-issue — clearest mental model but adds a synthetic project row
   - => recommend column on filter; falls back to "Inbox" virtual project if NULL
4. [ ] `internal/mailbox/service.go` — `Service.AutoCreateIssue(ctx, ingest Ingest, filter Filter, bodyText string) (Issue, error)`. Calls `projects.Service.CreateIssue` with the system-user identity. Inserts `email_ingest_issue`
5. [ ] `internal/mailbox/poll.go` — after `InsertIngest+InsertAttachments`, if `filter.to_issue`, fetch text body and call `AutoCreateIssue`. Guarded by `email_ingest_issue` row existence (idempotent — re-running the poll doesn't double-create)
6. [ ] `web/templ/inbox.templ` — when an ingest has `email_ingest_issue`, show "Issue created → #P/I" badge linking to the issue page
7. [ ] System user bootstrap — at boot, if `MAILBOX_SYSTEM_USER_ID` env is unset, INSERT a `users` row with display name "Mailbox" and email "mailbox@local"; persist its id back into a `mailbox_account.system_user_id` column? Or rely on env? Spec says env. Keep env. Bootstrap a row if missing
8. [ ] Tests `internal/mailbox/service_test.go` — html→text conversion fixtures, duplicate-call idempotency

Verification: register a `to_issue=true` filter for `support@vendor.tld` → community A → project P. Send an HTML-only email from that address → after next poll, project P has a new issue with the plaintext body, no raw `<div>`s, editable via the existing issue edit handler.

### Phase 8 — Admin filter CRUD UI — status: open

Goal: an admin of a community can list / add / delete filters for THAT community through a UI. Pattern routes are per-community (`/c/{slug}/admin/mail-filters`) even though the inbox is global — filters are owned by communities.

1. [ ] `internal/mailbox/handler.go` — `GetFilters`, `PostCreateFilter`, `PostDeleteFilter`. RequireMember + RequireAdmin middleware on the route group
2. [ ] `internal/mailbox/repo.go` — `ListFiltersForCommunity`, `InsertFilter`, `DeleteFilter`
3. [ ] `web/templ/mailfilters.templ` — page with two tables (domain / address), inline new-row forms, delete button per row
4. [ ] On any filter mutation: invalidate the in-memory filter cache in `internal/mailbox/filter.go`. The cleanest signal is a small Bus on the repo
5. [ ] Sidebar entry in the community admin nav: "Mail filters" link visible when `MAILBOX_ENABLED && viewer.Role >= admin`

Verification: as admin of community A, navigate to `/c/A/admin/mail-filters` → see two empty tables → add a domain filter `acme.com` (handler stores as `@acme.com`) → run poll worker → an `acme.com` sender matches and writes a row tagged to community A. Delete the filter → next poll sender from `acme.com` no longer matches.

### Phase 9 — Deferred — status: open

Not implemented in this plan. Reserved here so spec's Future bullets don't lose traceability.

- {[?] IMAP IDLE for sub-second arrival latency.}
- {[?] OAuth (Google / Microsoft) instead of plain LOGIN.}
- {[?] Per-attachment search across discarded emails — forensic recovery.}
- {[?] Filter-on-subject and filter-on-recipient (To/Cc) headers in addition to From.}
- {[?] "Show consumed" toggle on inbox to re-surface processed emails.}
- {[?] "Show discarded" toggle for forensic search.}
- {[?] Inline body preview — fetch text/plain on-demand inside the inbox row.}
- {[?] Reply-to-create-thread flow (chat / forum / discussion) once write support is added.}

## Verification

End-to-end acceptance:

1. **Read-only** — after 24 hours of continuous polling against a mailbox a user is reading via Thunderbird, message `\Seen` flags remain whatever the user set them to. No surprise read-marks.
2. **Idempotent** — restart the app mid-cycle, restart between cycles, restart inside an INSERT — no duplicate `email_ingest` rows, no duplicate `project_attachments` rows from a single "Move" click, no duplicate `project_issues` from a `to_issue=true` filter.
3. **Filter precedence** — `alice@acme.com` (exact, community B) wins over `*@acme.com` (domain, community A) — verified by sending from `alice@acme.com` and seeing the row in B only.
4. **UIDVALIDITY rotation** — flip the IMAP backing UIDVALIDITY of a folder (greenmail supports this) → that folder is rescanned from UID 0 next cycle; other folders untouched.
5. **Lazy fetch contract** — packet-capture (`socat` / `mitmproxy`) confirms no `BODY[]` IMAP command during poll. Only `BODYSTRUCTURE` + ENVELOPE. `BODY.PEEK[<part>]` issues only on "Move" click.
6. **HTML→text auto-issue** — issue body is markdown-clean text, no raw HTML.
7. **Infinite scroll** — 250 ingested rows in DB → user sees 100 → scrolls → 100 → scrolls → 50 → sentinel disappears.
8. **Anti-enumeration** — non-admin user receives 403 on `/inbox` and `/inbox/more?community=<id>` for any community they aren't admin in.
9. **CI grep gate** — `make lint-mailbox` returns no hits.

## Adjustments

- `2606151045` — User added "click sender → attach to community" affordance after first commit. Phase 0 step 2 now covers the spec edit; Phase 5 step 6 implements it in the inbox UI; Phase 8 step 1 shares the underlying `PostCreateFilter` handler. No phase added — fits inside existing surfaces.

## Progress Log

- `2606151040` — Plan drafted off `task/spec-mailbox-imap-ingest`. Spec already committed at `1741fd7`. User clarified global inbox shape; Phase 0 captures the spec refinement before any code lands.
- `2606151105` — Phase 0 done. Spec refined inline via `/eidos:refine`: §Global inbox replaces §Sorting queue, click-sender popover added, anti-enumeration tightened, Future bullet updated. No code yet — implementation starts at Phase 1.
- `2606151140` — Phase 1 done. Migration 00020 + 8 config envs + `internal/mailbox` (types/repo/handler/cursor_test) + `internal/auth/AdminCommunityIDs` + `web/templ/inbox.templ` + topbar wiring. All tests green (`go test ./...`). Empty `/inbox` reachable behind the flag for admins of any community; anti-enum 404 elsewhere.
