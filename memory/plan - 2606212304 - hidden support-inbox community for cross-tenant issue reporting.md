---
name: plan-support-inbox-write-only-community
status: active
type: plan
tldr: A single hidden "support" community acts as a write-only, cross-tenant issue inbox. Any authed member reports an issue from a global topbar button; the report lands as a project_issue in the support community's Inbox project. The reporter can read back ONLY their own reports + replies (mini support ticket). Only super-admins read the full inbox (existing god-mode). Env-configured slug, boot-seeded, no migration.
---

# Plan — hidden support-inbox community (cross-tenant write-only issue reporting)

## Context

User runs **community-per-customer**: each customer is an isolated tenant who must
not see or know about other customers. The whole architecture is per-tenant
isolation (membership gates every read). There is no surface for a customer to
report a problem to the platform operator.

Goal: ONE hidden community that is a **write-only inbox** — customers write issues
in; nobody reads except the platform super-admin. Reporter can read back their own
reports + super-admin replies.

### Resolved decisions (asked 2026-06-21)
1. **Designation** = env-hardcoded slug, boot-seeded (`SUPPORT_INBOX_SLUG`). No
   migration. Mirrors `community.BootstrapOrFetch` (`cmd/app/main.go:104`).
2. **Who reports** = authed members only (topbar button when signed in).
3. **Write-only strictness** = reporter reads back **own** reports + replies
   (mini support-ticket thread). Bigger build than fire-and-forget.

### Reuse (Agile / DRY — no new domain tables)
- A **report = a `project_issue`**; a **reply = a `project_issue_comment`**.
  Full reuse of `projects` issues machinery.
- `projects.Service.CreateIssue(projectID, title, body, creator)` —
  `internal/projects/issues_service.go:32`.
- `projects.Service.AddIssueComment(...)` — `issues_service.go:133`.
- `projects.Repo.ListIssueComments` / `InsertIssueComment` —
  `internal/projects/issues_comments_repo.go:13,74`.
- Inbox-project seeding pattern: `mailbox.ensureInboxProject` —
  `internal/mailbox/service.go:457` (find-or-create project titled "Inbox").
- `projects.Identity` maps an auth user → issue creator
  (`internal/projects/guest.go`).
- **Super-admin reads the full inbox via existing god-mode** — open
  `/c/<support-slug>/projects/<inbox>/issues` (super-admin bypass in
  `community.RequireMember`, see `internal/superadmin/CLAUDE.md` §5d). No new
  admin surface needed.

### Why write-only is (almost) free
The reporter never receives a `memberships` row in the support community. Every
GET under `/c/<support>/...` is membership-gated → 401 for them. The support
community never appears in their dashboard (`ListForUser` is membership-joined),
explore (`is_public=0` default), or `/issues` picker (admin-membership gated). So
the ONLY surfaces we must hand-build are:
- a global **write** endpoint (`POST /report-issue`), and
- a global **own-reports read-back** surface, both scoped to `creator_user_id ==
  caller`.

### Conflict surfaced
Spec intent (per-tenant isolation, `eidos/spec - forumchat`) vs a deliberate
cross-tenant channel. Safe because it is one-directional (write-in) and read is
super-admin-only + own-only. Worth recording as an explicit exception → Phase 0
spec.

### Relevant specs
- [[spec - project-issues - per-project-issues-with-guest-share-links]]
- [[spec - mailbox - imap-ingest-to-per-community-queue]]
- [[spec - projects - per-community-collaborative-projects]]

## Decisions still open (resolve before/while coding)
- **D1 — package home for the new handler.** It imports `projects` (issues) +
  `community` (resolve support community) + `auth`. Options: (a) new tiny
  `internal/support` package; (b) methods on `projects.Handler` (already holds
  `Repo`, `Svc`, `AuthRepo`). Lean (a) `internal/support` — keeps `projects`
  free of the cross-tenant concept and the "only-own-reports" auth rule lives in
  one place. **Recommend (a).**
- **D2 — super-admin reply visibility to reporter.** Reporter's read-back lists
  all non-deleted comments on their own issue, including super-admin replies.
  Super-admin name shows as their display name (fine). No extra flag.
- **D3 — should the support community be hidden from the super-admin `/issues`
  picker / explore?** It is `is_public=0` (hidden from explore). It will appear
  in the super-admin `/superadmin` community roster (correct — they manage it).
  It must NOT leak into any normal user's `/issues` picker — it won't, because
  that is gated on `AdminCommunityIDs` (admin membership), which reporters lack.
  No code needed; verify in tests.

## Phases

### Phase 0 — spec the exception (status: open)
0.1 [ ] `/eidos:spec support-inbox` — write
    `eidos/spec - support-inbox - hidden-cross-tenant-write-only-issue-inbox.md`
    capturing: the single hidden community, env slug, write-only guarantee via
    no-membership, own-reports read-back, super-admin-only full read, and the
    isolation exception. (Short spec; this is a real architectural exception.)

### Phase 1 — config + boot-seed the support community (status: open)
1.1 [ ] Add `SupportInboxSlug string` (env `SUPPORT_INBOX_SLUG`, default `""` =
    feature OFF) and `SupportInboxName` (default `"Support"`) to
    `internal/config/config.go`. Empty slug ⇒ feature disabled (no button, no
    routes) — same flag-gate shape as `ProjectsEnabled`.
    - => verify: `go build ./...`.
1.2 [ ] In `cmd/app/main.go run()`, after `bootCommunity`, when
    `cfg.SupportInboxSlug != ""`: `cRepo.BootstrapOrFetch(ctx, slug, name)` →
    support community; then find-or-create its **Inbox** project via
    `projectsSvc`/`projectsRepo` (reuse the `ensureInboxProject` shape — extract
    a shared helper to avoid a 2nd copy, see D-DRY below). Log
    `support inbox ready`.
    - => DRY: `mailbox.ensureInboxProject` is private. Extract a reusable
      `projects.Service.EnsureNamedProject(ctx, communityID, creatorID, title,
      desc) (projectID, error)` and have BOTH mailbox and the support boot call
      it. (Refactor commit, separate from feature commit.)
    - => creatorID for the support Inbox project: needs a real users(id) (FK).
      Reuse `cfg.MailboxSystemUserID` if set, else the first super-admin's user
      id, else skip seeding with a warning. Decide in 1.2.
    - => verify: boot with `SUPPORT_INBOX_SLUG=support` → community + Inbox
      project exist; boot without it → nothing created.

### Phase 2 — write path: global "Report issue" (status: open)
2.1 [ ] New `internal/support` package (D1): `Handler` holding `Community
    *community.Repo`, `Issues *projects.Service`, `Projects *projects.Repo`,
    `AuthRepo *auth.Repo`, `Log`, plus resolved `slug`. A small
    `resolveInbox(ctx) (communityID, projectID, error)` (cached) finds the
    support community by slug + its Inbox project.
2.2 [ ] `GET /report-issue` — render the report composer page (Layout + a
    title+body form; datastar signals `report_title`, `report_body`). Authed.
    Also lists the caller's OWN past reports (Phase 3 surface) on the same page.
2.3 [ ] `POST /report-issue` — read signals BEFORE `NewSSE`; build
    `projects.Identity` from `auth.FromContext`; stamp reporter's **home
    community name** into the body (triage context) — resolve via
    `cRepo`/`AuthRepo`; call `Issues.CreateIssue(inboxProjectID, title, body,
    creator)`; return SSE: clear signals + morph the "my reports" list +
    a "thanks" confirmation fragment.
    - => verify: member A posts a report → row in `project_issues` under the
      support Inbox project with `creator_user_id = A`.
2.4 [ ] Topbar button in `web/templ/layout.templ` (`if v.IsAuthed &&
    SupportInboxEnabled(ctx)`): `🛟 Report issue` → `/report-issue`. Add a
    `SupportInboxEnabled` ctx flag mirroring `IsAdminOfAnyCommunity`/`AIEnabled`
    package-var + ctx pattern (leaf-package rule §4.13).
    - => `make gen` after editing the templ.

### Phase 3 — read-back: own reports + replies (status: open)
3.1 [ ] `GET /report-issue/{iid}` — load issue; **403 unless `issue.CreatorUserID
    == caller.UserID`** (the load-bearing guard); confirm the issue's project is
    the support Inbox project. Render issue body + `ListIssueComments` thread +
    a reply composer. Reuse issue-comment view mapping.
3.2 [ ] `POST /report-issue/{iid}/reply` — same own-only guard, then
    `Issues.AddIssueComment(inboxProjectID, iid, caller, body)`; morph the
    thread. (No status UI for the reporter — status stays super-admin-only via
    the normal issues page.)
3.3 [ ] Realtime (optional, can defer): subscribe the report-detail page to the
    per-project issues Bus event so a super-admin reply appears live. If
    deferred, a manual refresh shows it — document as friction.
    - => verify: super-admin replies on the issue via
      `/c/<support>/projects/<inbox>/issues/<iid>` → reporter sees it on
      `/report-issue/<iid>` (after refresh if 3.3 deferred); reporter replies →
      super-admin sees it.

### Phase 4 — routing, wiring, isolation tests (status: open)
4.1 [ ] Mount the routes in `cmd/app/main.go` inside an authed group
    (`r.Use(auth.RequireAuth)`), gated by `cfg.SupportInboxSlug != ""` — sits
    next to the `/issues` group (`main.go:1649`). Construct `support.Handler`
    near the projects handler (`main.go:321`).
4.2 [ ] Tests (`internal/support/handler_test.go`, sqlite `t.TempDir()`):
    - report create lands in the support Inbox project with correct creator.
    - read-back lists ONLY the caller's own reports (member B can't see A's).
    - `GET /report-issue/{A's iid}` as B → 403.
    - reporter cannot reach `/c/<support>/...` (no membership) — assert
      `RequireMember` 401 (or document as covered by existing middleware).
    - support community absent from a normal member's `ListForUser`.
4.3 [ ] CSS for the report page + "my reports" list + topbar button icon
    (reuse issue/inbox design tokens, per [[feedback_ux_first]]). Screenshot
    before done.

## Verification (success criteria)
- `SUPPORT_INBOX_SLUG=support` boots → hidden `support` community + Inbox project.
- Authed member sees `🛟 Report issue` in topbar; opens composer; submits →
  "thanks"; the report appears in their "my reports" list.
- Member B cannot see member A's report (list-scoped + 403 on direct id).
- No reporter is ever a member of `support`; it never shows in their dashboard,
  explore, or `/issues` picker.
- Super-admin opens `/c/support/projects/<inbox>/issues` (god-mode) → sees ALL
  reports; replies; reporter sees the reply.
- Feature fully OFF when `SUPPORT_INBOX_SLUG=""` (no routes, no button).
- `go build ./...`, `go test ./...`, `make gen` all green.

## Friction / trade-offs
- Reporter's home-community context is stamped into the issue body text (not a
  structured column) — cheapest, readable in the super-admin issues UI.
- Super-admin uses the EXISTING issues UI to triage (no bespoke admin inbox) —
  intentional reuse; they reach it via god-mode, not a nav link.
- If Phase 3.3 realtime is deferred, reporter sees replies on refresh only.
- One global inbox = all customers' reports mixed in one project. If volume
  grows, a future iteration could auto-create one Inbox project per home
  community inside the support community (still one hidden community). Out of
  scope now.

## Mapping (to fill as built)
> [[internal/config/config.go]]
> [[cmd/app/main.go]]
> [[internal/projects/service.go]] (EnsureNamedProject extraction)
> [[internal/support/handler.go]]
> [[web/templ/support.templ]]
> [[web/templ/layout.templ]]
> [[eidos/spec - support-inbox - hidden-cross-tenant-write-only-issue-inbox]]

## Progress Log
- 2606212304 — Plan created. Context loaded (effective-go, eidos specs, code
  graph, mempalace). Decisions D-designation/who/read-back resolved with user.
  Branch `task/support-inbox-write-only-community`.
- 2606212320 — Phases 1–4 implemented + verified. Commits:
  - `7ae21f3` refactor: extract `projects.Service.EnsureNamedProject`
    (DRY; mailbox delegates to it). D1 → chose new `internal/support` package.
  - `cc8a97b` config: `SUPPORT_INBOX_SLUG` / `SUPPORT_INBOX_NAME` (off by default).
  - `31d6c70` feat: `internal/support` handler (write + own-only read-back),
    `projects.Repo.IssuesByCreator`, `web/templ/support.templ`, layout 🛟 link,
    main.go boot-seed + route mount.
  - `4a99852` test: owner-scoped read-back, `ownedIssue` guard, triage stamp (4 tests, green).
  - `593f30c` style: `.support-*` CSS (token-based, dark-safe).
  - => **Lesson (cost a debug cycle):** never call raw `datastar.NewSSE` in a
    handler — this app's chi compressor garbles the SSE body unless headers are
    primed first. ALWAYS use `render.NewSSE(w, r)` (it calls `httpx.PrimeSSE`).
    Symptom: handler runs + DB write lands, but the client applies no patches +
    a "superfluous response.WriteHeader" log line. Fixed both POST handlers.
  - => Runtime-verified with Playwright (register → report → reply): green
    "Thanks" flash, owner-scoped list, triage blockquote (reporter + home
    community), "You" reply bubble. Screenshots captured.
  - => Inbox project is created lazily on first report (creator = first
    reporter) — avoids needing a valid users(id) FK at boot on a fresh DB.
- Phase 0 (spec) deferred — user chose "start coding". The cross-tenant
  isolation exception still warrants `eidos/spec - support-inbox`; left as the
  one open follow-up.
