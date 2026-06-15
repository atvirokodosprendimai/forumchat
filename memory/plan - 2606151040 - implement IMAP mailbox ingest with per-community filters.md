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

### Phase 2 — IMAP poll loop shell, all folders, no DB writes — status: completed

Goal: with valid `MAILBOX_*` env vars set, the poll worker dials IMAP every `MAILBOX_POLL_INTERVAL`, EXAMINEs every folder, logs envelope info. No row writes. Lets us verify connectivity, read-only behaviour, and folder enumeration before adding any state.

1. [x] `go get github.com/emersion/go-imap/v2 github.com/emersion/go-message` + go-sasl via tidy.
2. [x] `internal/mailbox/imap.go` — `imapClient` wrapper with `dial`, `close`, `listFolders`, `examineReadOnly` (forces `ReadOnly:true`), `fetchEnvelopesSince(uid)`. Single file holds every IMAP method so the CI grep gate has one target.
3. [x] `internal/mailbox/poll.go` — `PollWorker{Cfg, Interval, Log}` with `Start(ctx)` spawning a goroutine; per cycle Dial → listFolders → for each folder examineReadOnly + fetchEnvelopesSince → log results → close. First cycle fires immediately so boot success/failure is visible without waiting a full interval.
4. [x] `cmd/app/main.go` — when `MAILBOX_ENABLED=true` and host/user set, build the worker with `digestCtx` (shared with other interval workers) and `.Start()`.
5. [x] CI grep gate — `make lint-mailbox` enforces `! grep -rnE 'Store\(|Expunge|\.Move\(|\.Copy\(|BodySection\{[^}]*Peek:[[:space:]]*false' internal/mailbox/`. Verified passing.

Verification (manual smoke deferred until a test IMAP container is available): `MAILBOX_ENABLED=true` against greenmail or `dovecot` shows one Dial per interval, folder enumeration, UID fetches. Thunderbird verification confirms `\Seen` flags unchanged.

### Phase 3 — Filter table + matching + email_ingest writes — status: completed

Goal: emails matching any `community_mail_filter` get persisted; non-matches are silently skipped. Idempotent.

1. [x] `internal/mailbox/filter.go` — `MatchFrom(ctx, repo, fromAddr) (Filter, ok, error)` implementing precedence (exact address > wildcard domain). Also `normaliseFilterPattern` helper used by Phase 8's CRUD.
2. [x] `internal/mailbox/repo.go` — added `cachedFilters` + `InvalidateFilters` (RWMutex-guarded in-memory cache), `UpsertFolder` (handles UIDVALIDITY rotation by resetting last_uid to 0), `SetFolderLastUID` (monotonic), `InsertIngest` (FindByUID pre-check + UNIQUE constraint absorbing duplicates, surfaces `isNew bool`).
3. [x] `internal/mailbox/poll.go` — `scanFolder` now match-then-persist per envelope, advances `last_uid` once per folder cycle.
4. [x] `cmd/app/main.go` — PollWorker now constructed with `AccountID` + `Repo` so it can persist.
5. [x] Tests `internal/mailbox/filter_test.go` — precedence cases (exact > domain, case-insensitive), malformed/empty from, normalisation cases.
6. [x] Tests `internal/mailbox/repo_test.go` — UIDVALIDITY rotation resets cursor, `SetFolderLastUID` is monotonic, duplicate `InsertIngest` returns isNew=false with original id, `EnsureAccount` updates singleton on config change.

Verification: `go test ./internal/mailbox/...` passes; `make lint-mailbox` passes.

### Phase 4 — Attachment metadata from BODYSTRUCTURE — status: completed

Goal: every matched email's attachments are indexed with metadata only (no bytes). The queue view can render filenames/sizes/mimes without ever having pulled the file body.

1. [x] `internal/mailbox/imap.go` — Fetch now requests BODYSTRUCTURE (extended). New `walkAttachmentParts(bs) []ParsedPart` returns `{Filename, MIME, SizeBytes, MIMEPartID}`. Attachment heuristic: `Content-Disposition: attachment` OR any non-multipart non-text part with a filename.
2. [x] `internal/mailbox/repo.go` — `InsertAttachments(ctx, ingestID, parts)` batch insert inside a transaction.
3. [x] `internal/mailbox/poll.go` — after `InsertIngest` returns `isNew=true`, call `InsertAttachments`. Duplicates skip the attachment write entirely (the metadata is already there from a previous cycle).
4. [x] Tests `internal/mailbox/imap_test.go` — multipart/mixed (PDF + inline-named PNG), nested multipart/alternative (text alternatives skipped, zip survives at part "2"), text-only mail produces no attachments, `formatPath` numbering.

Verification (manual smoke deferred to integration test stage): `go test ./internal/mailbox/...` covers the parser + repo paths.

### Phase 5 — Global inbox queue UI with community filter pills + infinite scroll — status: completed

Goal: `/inbox` renders last 100 ingest rows the admin can see, with community filter pills along the top. Scroll-end fetches the next 100.

1. [x] `internal/mailbox/repo.go` — `QueueForViewer` from Phase 1 is reused. `InsertFilter` + `DeleteFilter` + `ListFiltersForCommunity` added so the popover shares its handler with Phase 8.
2. [x] `internal/mailbox/handler.go` — `GetMore` parses `?cursor=` + `?community=`, returns SSE that appends rows + replaces the `#inbox-more` sentinel. `GetStream` is the long-lived SSE the inbox page opens once: subscribes to every admin-of community's Bus + NATS subject, re-renders the first page on any signal with a 25 s keepalive.
3. [x] `web/templ/inbox.templ` — `InboxPage` (full), `InboxRowList`, `InboxMore`, `InboxAttachDialog` extracted; sender chip carries `data-on:click="$attach_addr='<from>'; $attach_open=true; …"`. Popover is a `<dialog>`-style modal with kind toggle, community select, to-issue checkbox, Save/Cancel buttons.
4. [ ] `web/static/app.css` — minimal styling deferred; the modal works on the layout's default styles.
5. [x] `internal/mailbox/bus.go` — per-community `Bus`. `PollWorker.broadcast` and `Handler.broadcast` both call it; NATS subject is `community.<cid>.mailbox` (new `natsx.MailboxSubject`).
6. [x] `PostAttachSender` — reads `$attach_addr / $attach_kind / $attach_community / $attach_to_issue`; verifies the viewer is admin in the chosen community; normalises the pattern; inserts the filter via the same `Repo.InsertFilter` Phase 8's CRUD page will use; clears the signals on success.

Verification: `go test ./...` green; manual smoke deferred until an IMAP test container drops mail in.

Verification: load `/inbox` with 250 rows in DB → see first 100 → scroll to bottom → 100 more append → scroll again → 50 more append + sentinel hides. Click a community pill → first 100 of that community only. Run poll worker, send a new email matching a filter → list morphs in within ~1s.

### Phase 6 — Lazy attachment materialise → project doc — status: completed

Goal: from the inbox, an admin picks a project + category from per-attachment dropdowns and clicks "Move". The chosen attachment is fetched (BODY.PEEK[mime_part_id]), saved to uploads, and linked into `project_attachments`.

1. [x] `internal/mailbox/handler.go` — `PostMoveAttachment` — reads JSON body `{project_id, category}`, authorises via `AdminCommunityIDs` against the attachment's community, delegates to `Svc.Materialise`, broadcasts on success, returns 204.
2. [x] `internal/mailbox/service.go` — `Service.Materialise` opens a short-lived IMAP session, EXAMINEs the right folder, calls `imapClient.fetchPart(uid, mime_part_id)` (BODY.PEEK), pipes the bytes to `projects.Service.AddAttachment`, stamps the mailbox attachment row, flips ingest status when every attachment is moved.
3. [x] `internal/mailbox/repo.go` — `AttachmentByID` returns the attachment + parent ingest + folder name in one SELECT JOIN. `MarkAttachmentMoved` records upload+project+category+timestamp guarded by upload_id IS NULL. `MarkIngestConsumedIfAllMoved` flips status when no nullable upload_id rows remain.
4. [x] `web/templ/inbox.templ` — per-attachment `inboxMoveForm` with project `<select>` (scoped to row's community) + free-text category + submit button. Form POSTs JSON to `/inbox/attachments/{id}/move`; the open SSE stream re-renders the rows on Bus broadcast.
5. [x] After-move SSE — `Bus.Broadcast(communityID)` + NATS publish. Open inbox tabs morph.
6. [x] Cross-community guard — `Svc.Materialise` rejects when the chosen project's `community_id` doesn't match the ingest's. Authorisation also checked at the handler boundary via `AdminCommunityIDs`.

Verification: `go test ./...` green, `make lint-mailbox` green. Manual: queue has email with 2 attachments, pick project + category, click Move → file lands in project docs, ingest row updates via SSE morph. When all attachments moved, ingest status flips to consumed and row drops out of the default queue.

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
7. [ ] System user bootstrap — when `MAILBOX_SYSTEM_USER_ID` env is empty, fall back to "longest-tenured admin of each community" (per-community resolution at issue-create time). The auto-issue's `creator_user_id` becomes that admin's user id. Document the fallback in spec Notes. Avoids the need for a synthetic users row entirely.
   - => user direction (2606151207): "can this be `global admin`? we don't have any system user. Automatic chooses global admin if not preset". So env-unset path picks an admin per community at write time, not at boot.
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
- {[!] Email search — SQLite FTS5 over (subject, from_addr, from_name, body_text, attachment filenames concatenated). Query "api" matches subject keywords, sender names, body content, AND filenames like "api documentation.doc". Per user request 2606151225. Lands after Phase 5 (UI exists to expose the search box) but before Phase 8. Body text gets persisted into `email_ingest.body_text` at poll time for matched messages so search has something to index without re-fetching from IMAP.}

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
- `2606151205` — Phase 2 done. `internal/mailbox/imap.go` wraps emersion/go-imap/v2 with READ-ONLY guarantees baked in (EXAMINE not SELECT, BodySection unused). `PollWorker.Start(ctx)` runs an immediate-first ticker that logs folder + envelope info per cycle. `make lint-mailbox` gates merges against any forbidden mutating call landing in `internal/mailbox/`.
- `2606151207` — User clarified MAILBOX_SYSTEM_USER_ID semantics: when unset, fall back to "global admin of the community" at issue-write time. Plan Phase 7 step 7 updated; no code yet — wires in when auto-issue lands.
- `2606151220` — Phase 3 done. filter.go + cachedFilters cache + UpsertFolder + InsertIngest + idempotent scanFolder. Tests cover precedence, rotation, monotonic cursor, duplicate absorption. lint-mailbox green.
- `2606151225` — User requested email search across content + attachment filenames. Filed as new Future-but-must-do bullet (`{[!]}`). Will land as a new Phase 5b between queue UI and Phase 6 — once UI exists to expose the search box. Implementation note: persist text body into `email_ingest.body_text` at poll time and build SQLite FTS5 virtual table over (subject, from_addr, from_name, body_text, attachment filenames). No IMAP refetch.
- `2606151235` — Phase 4 done. `walkAttachmentParts` + `InsertAttachments` + poll wiring + tests covering nested multipart numbering and the text-only-no-attachments case. All tests pass, lint-mailbox green.
- `2606151310` — Phase 5 done. Bus + Handler.GetMore/GetStream/PostAttachSender + natsx.MailboxSubject + InsertFilter/DeleteFilter/ListFiltersForCommunity + InboxRowList/InboxMore/InboxAttachDialog templates + InitialSignals extended for attach + inbox signals. Routes wired in main.go behind the flag. CSS polish deferred (Phase 9 cosmetic). Tests still green.
- `2606151345` — Phase 6 done. imap.fetchPart (BODY.PEEK), Service.Materialise, AttachmentByID JOIN lookup, MarkAttachmentMoved + MarkIngestConsumedIfAllMoved repo methods, PostMoveAttachment handler with admin-of-community guard, per-attachment Move form in inbox template, route + service wired. lint-mailbox green; tests green.
