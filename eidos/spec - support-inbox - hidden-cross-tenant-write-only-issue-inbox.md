---
name: spec-support-inbox-hidden-cross-tenant-write-only-issue-inbox
status: shipped
type: spec
tldr: ONE hidden community (env SUPPORT_INBOX_SLUG) is a write-only, cross-tenant issue inbox. Any signed-in member files a report from a global "Report issue" button; it lands as a project_issue in that community's "Inbox" project, stamped with the reporter's home community. Reporters never join the community, so they can only read back their OWN reports + replies; only platform super-admins read the whole inbox, via a dedicated /support-inbox surface. Reuses the projects issues machinery — no new tables.
---

# Support inbox — hidden cross-tenant write-only issue reporting

## Target

forumchat runs **community-per-customer**: each customer is an isolated tenant
who must not see, know about, or talk to other customers. Membership gates every
read; there is no surface for a customer to reach the platform operator.

This spec adds the one deliberate **cross-tenant exception**: a single hidden
community that is a **write-only inbox**. Customers write reports in from
anywhere; nobody reads except the platform super-admin (and the reporter, their
own reports only).

It is **off by default** — a fresh deployment has no support inbox at all.

## Behaviour

### The hidden community
- Designated by env `SUPPORT_INBOX_SLUG` (display name `SUPPORT_INBOX_NAME`,
  default `Support`). Empty slug ⇒ feature OFF: no nav links, no routes, nothing
  seeded.
- Boot-seeded with `community.BootstrapOrFetch` (idempotent). `is_public=0` so it
  never appears in explore. It holds one project, **"Inbox"**, created **lazily**
  on the first report (creator = first reporter) — avoids needing a valid
  `users(id)` FK at boot on a fresh DB.

### A report is an issue; a reply is a comment
- Reusing the projects issues machinery wholesale (no new domain tables): a
  report is a `project_issues` row in the Inbox project; a reply is a
  `project_issue_comments` row; status is the issue's status.

### Who can write
- Any **signed-in** member, from a global **"Report issue"** nav link
  (🛟, shown when `SUPPORT_INBOX_SLUG` is set). `RequireAuth` only — **not**
  `RequireApproved`, so even a member stuck in the pending queue can reach out.
- The composer takes a subject + details. On submit the report is authored by the
  caller and the body is prefixed with a triage header — reporter name, email,
  and **home community** (name + slug) — so the super-admin knows which tenant
  filed it.

### Write-only is structural, not a check
The reporter **never receives a `memberships` row** in the support community.
Therefore, with zero extra enforcement:
- Every `GET /c/<support-slug>/...` is membership-gated → 401 for them. They
  cannot browse the inbox.
- The community never appears in their dashboard (`ListForUser` is
  membership-joined), explore (`is_public=0`), or the global `/issues` picker
  (gated on `AdminCommunityIDs`).

The only two things a reporter can do are file a report and read back their own.

### Reporter read-back (own only)
- `/report-issue` lists the caller's own reports; `/report-issue/{iid}` shows one
  report + its conversation + a reply box. Both scoped to
  `creator_user_id == caller`.
- The **load-bearing guard** `accessibleIssue` returns not-found unless the issue
  is in the Inbox project AND (caller is the creator OR caller is super-admin). A
  not-owned / unknown id is a **404, never 403** (anti-enumeration).

### Super-admin read (whole inbox)
- Only platform super-admins read all reports. A dedicated, discoverable
  **`/support-inbox`** (📨 nav link, `RequireSuperAdmin`) lists every report
  across all tenants with the reporter's name.
- This surface is **self-contained**: it reads via the projects repo/service
  directly and does **not** require `PROJECTS_ENABLED`. (The pre-existing
  god-mode path `/c/<support-slug>/projects/<inbox>/issues` still works *if*
  PROJECTS_ENABLED is on, but is not the intended route.)
- A super-admin opens any report through the same `/report-issue/{iid}` detail
  (via the `accessibleIssue` bypass), replies, and changes status
  (`POST /report-issue/{iid}/status?to=…`, super-admin only) — the reporter sees
  the new status + reply on their next view.

### Explicit non-goal
The support community is **not** folded into the global `/issues` page. That
page's picker is for *opening* new issues into communities you administer;
surfacing the hidden inbox there would expose it as a target and break the
per-tenant model. The read path is `/support-inbox`, by design.

## Design

### Package layout
```
internal/support/
  handler.go        GetReport/PostReport, GetReportDetail/PostReply (own-or-admin),
                    GetInbox + PostStatus (super-admin), accessibleIssue guard,
                    lazy Inbox resolution, triage-header composeBody, view mappers
  handler_test.go   owner-scoped read-back, accessibleIssue (+ super-admin bypass,
                    non-Inbox rejection), triage stamp
web/templ/
  support.templ     composer + my-reports list + inbox + ticket thread + status bar
```

`internal/support` is the seam: it imports `projects` (issues) + `community` +
`auth` + `render` so none of those import the cross-tenant concept. The
"only-own-reports" / super-admin-bypass rule lives in exactly one place
(`accessibleIssue`).

### Reuse / DRY
- `projects.Service.EnsureNamedProject(communityID, creatorID, title, descMD)` —
  extracted find-or-create-by-title, shared by `mailbox.ensureInboxProject` and
  the support Inbox seed.
- `projects.Repo.IssuesByCreator(projectID, creatorUserID)` — the creator-scoped
  read-back query.
- `projects.Service.CreateIssue / AddIssueComment / UpdateIssueStatus` and
  `projects.Repo.ListIssues / ListIssueComments` — unchanged, reused as-is.

### SSE
Every action handler opens SSE with `render.NewSSE` (which primes headers via
`httpx.PrimeSSE`), **never** raw `datastar.NewSSE` — otherwise chi's compressor
gzips the un-primed body and the browser applies no patches.

### Routes
```
GET  /report-issue                     composer + own reports        (RequireAuth)
POST /report-issue                     file a report                 (RequireAuth)
GET  /report-issue/{iid}               one report (own or admin)     (RequireAuth)
POST /report-issue/{iid}/reply         reply (own or admin)          (RequireAuth)
GET  /support-inbox                    all reports                   (RequireSuperAdmin)
POST /report-issue/{iid}/status?to=…   change status                 (RequireSuperAdmin)
```

## Verification
- `SUPPORT_INBOX_SLUG=support` boots → hidden `support` community + (lazy) Inbox
  project. Empty slug → no routes, no nav links, nothing seeded.
- Member files a report → "Thanks" + it appears in their own list; the row lands
  in the support Inbox project authored by them, body carries the triage header.
- Member B cannot see member A's report (list scoped + `/report-issue/{A-id}` →
  404). A normal project issue is never reachable through `/report-issue` (the
  Inbox-project check), even for a super-admin.
- Super-admin clicks 📨 Support inbox → sees ALL reports (reporter name shown),
  opens one, changes status (live morph), replies → reporter sees the status +
  reply on reload. **Works with `PROJECTS_ENABLED` off.**
- `internal/support/handler_test.go` covers owner-scope, the super-admin bypass,
  non-Inbox rejection, and the triage stamp; `go test ./...` green. End-to-end
  validated with a two-context Playwright run.

## Friction / trade-offs
- One global Inbox project mixes all tenants' reports. If volume grows, a future
  iteration could auto-create one Inbox project per home community *inside* the
  one hidden community. Out of scope.
- Triage context (reporter + home community) is stamped as a markdown blockquote
  in the body, not a structured column — cheapest, readable in the existing UI,
  and visible to the reporter too (their own info, harmless).
- Reporter sees a super-admin reply / status change on **refresh**, not live
  (no SSE stream on the detail page yet).
- The first reporter "owns" the Inbox project (lazy creator) — invisible, since
  reporters can't reach the project surface.

## Interactions
- Depends on [[spec - project-issues - per-project-issues-with-guest-share-links]]
  — a report reuses `project_issues` + `project_issue_comments`.
- Depends on [[spec - projects - per-community-collaborative-projects]] for the
  Inbox project + `EnsureNamedProject`.
- Relates to [[spec - mailbox - imap-ingest-to-per-community-queue]] — same
  find-or-create "Inbox" project shape (now shared via `EnsureNamedProject`).
- Super-admin reach uses the platform super-admin model (env `SUPERADMIN_EMAILS`,
  `auth.RequireSuperAdmin`); see `internal/superadmin/CLAUDE.md` §5d.

## Mapping
> [[internal/config/config.go]] (SUPPORT_INBOX_SLUG / SUPPORT_INBOX_NAME)
> [[internal/support/handler.go]]
> [[internal/support/handler_test.go]]
> [[web/templ/support.templ]]
> [[web/templ/layout.templ]] (🛟 Report issue, 📨 Support inbox nav links)
> [[internal/projects/service.go]] (EnsureNamedProject)
> [[internal/projects/issues_repo.go]] (IssuesByCreator)
> [[cmd/app/main.go]] (boot-seed + route mounts under SUPPORT_INBOX_SLUG)
> [[memory/plan - 2606212304 - hidden support-inbox community for cross-tenant issue reporting]]

## Future
- {[?] reporter-side realtime — SSE on the detail page so super-admin replies /
  status changes appear without refresh}
- {[?] per-home-community Inbox projects inside the one hidden community, once
  report volume makes one mixed list unwieldy}
- {[?] email notification to the reporter when the super-admin replies}
- {[?] anonymous / guest reporting (currently authed members only)}
- {[?] structured reporter/home-community columns instead of the body blockquote}
