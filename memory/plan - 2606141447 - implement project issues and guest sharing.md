---
tldr: Build per-project Issues sub-resource and guest share-link sharing in 5 phases. Visible result at the end of each phase. Reuses the Projects spec's datastar virtual-DOM pattern. Guest write scope = create issues + comment on any issue + attach images.
status: active
---

# Plan: Implement project issues + guest share links per spec

## Context

- Spec: [[spec - project-issues - per-project-issues-with-guest-share-links]] (this branch)
- Parent: [[spec - projects - per-community-collaborative-projects]] (merged to main 2026-06-14)
- Patterns to reuse:
  - `internal/rooms/handler.caller()` — three-source identity resolver (auth user / guest / not-authed)
  - `internal/rooms/PublicRoutes` — invite landing mounted at root, no community context
  - `internal/projects/bus.go` + `service.go` + `handler.go` SSE-morph pattern (datastar virtual-DOM)
  - `internal/uploads.Store.Save` (image-whitelist path) — reused for issue attachments
- Plan-shape decisions:
  - Issues land BEFORE guest sharing so we never ship a guest-write path before the artifact it writes to exists.
  - Guest write-gating is one phase (Phase 4) that touches both issue routes and the project page (read-gating).
  - Each phase is one commit + one push; merge to main happens after Phase 5 unless we find an issue along the way.

## Phases

### Phase 0 — Refactor project page into MPA tabs — status: completed

Goal: each existing project panel (overview / todos / docs / comments / activity) becomes its own URL + tab. Functionality stays the same; the layout flips from one-page-many-panels to one-page-per-tab. This lands BEFORE issues so the issues tab plugs into the new shell.

1. [x] `web/templ/projects.templ` — added `ProjectTabs(slug, projectID, active)` strip + `projectShell` wrapper + 5 page templs (`ProjectOverviewPage`, `ProjectTodosPage`, `ProjectDocsPage`, `ProjectCommentsPage`, `ProjectActivityPage`); old `ProjectPage` removed
2. [x] `internal/projects/handler.go` — `loadProjectData` helper takes a wants-bitmap struct so each tab only fetches what it renders; `GetOverview` / `GetTodosTab` / `GetDocsTab` / `GetCommentsTab` / `GetActivityTab` replace `GetProject`
3. [x] `GetStream` was updated client-side via the data-init URL gaining `?tab=<active>`. Server still pushes all fragments — patches that target an absent selector silently no-op via morphdom, so no server change needed. Simpler than gating push by tab.
   - => Decision: server-push of all fragments is cheap (1 DB read per kind); the cost of complexifying GetStream isn't worth saving 4 morphs that no-op anyway.
4. [x] `cmd/app/main.go` — 4 new tab GET routes mounted; `GetOverview` keeps the `/projects/{id}` slot
5. [x] CSS — `.project-tabs` strip + `.project-overview-counts` pill list; `.project-body-single` drops the 2-col grid
6. [x] Issues tab href is rendered in the strip but the `GET /issues` route is not mounted yet — clicking it 404s until Phase 1 lands. Acceptable for a one-session ship.

Verification: visit `/c/<slug>/projects/<id>` → land on Overview with title, description, counts, and tab strip. Click `Todos` tab → see only todos. Realtime edits in one tab still propagate to other open tabs (SSE per-tab).

### Phase 1 — Issues data model + list + create (member-only) — status: open

Goal: any approved community member can open a new issue from the project page, see it land in the issues list, and click into the issue page. Status defaults to `open`. No comments, no attachments, no guests yet.

1. [ ] Migration `0001N_project_issues.sql` — `project_issues`, `project_issue_comments`, `project_issue_attachments`, `project_guest_invites` per spec (all four tables ship together)
2. [ ] `internal/projects/issues.go` — `Issue`, `IssueComment`, `IssueAttachment`, `Identity` (guest+auth shape), `IssueEvent` (kind: `issue` | `comments` | `attachments`)
3. [ ] `internal/projects/issues_repo.go` — `ListIssues(projectID, includeClosed bool)`, `IssueByID`, `InsertIssue`, `UpdateIssueTitle`, `UpdateIssueBody`, `UpdateIssueStatus`, `DeleteIssue`
4. [ ] `internal/projects/issues_service.go` — `CreateIssue`, `UpdateIssueStatus`, `DeleteIssue`; publish `IssueEvent` on success
5. [ ] `internal/projects/issues_handler.go` — `GetIssuesList` (panel under project page), `GetIssue`, `PostCreateIssue`, `PostUpdateStatus`, `PostDeleteIssue`. Member-only for now.
6. [ ] `web/templ/project_issues.templ` — `IssuesPanel(slug, projectID, issues)` (rendered inside ProjectPage), `IssuePage` (5-panel layout slimmed: header + body + comments + activity), `IssueStatusPill`
7. [ ] Wire under existing `PROJECTS_ENABLED` block in `cmd/app/main.go`; routes inside the existing auth-required community group
8. [ ] Project page templ — slot the new IssuesPanel after the todos section

Verification: member opens project page → clicks "Open new issue" → fills title + body → lands on `/projects/{id}/issues/{iid}` with status pill `open`. Status dropdown advances to `triaged`/`in_progress`/`closed`. Closed issues collapse under a `<details>`.

### Phase 2 — Issue comments + attachments (realtime) — status: open

Goal: members can comment on issues and attach inline images.

1. [ ] `issues_repo.go` — `ListIssueComments`, `InsertIssueComment`, `UpdateIssueComment`, `SoftDeleteIssueComment`, `IssueCommentByID`; `ListIssueAttachments`, `InsertIssueAttachment`, `IssueAttachmentByID`, `DeleteIssueAttachment`
2. [ ] `issues_service.go` — `AddIssueComment`, `UpdateIssueComment` (edit-grace), `DeleteIssueComment`; `AddIssueAttachment` (uses `uploads.Store.Save` — image whitelist path), `DeleteIssueAttachment`
3. [ ] `issues_handler.go` — `PostIssueComment`, `PostIssueCommentEdit`, `PostIssueCommentDelete`, `PostIssueAttachmentUpload`, `PostIssueAttachmentDelete`; per-issue SSE stream `GetIssueStream`
4. [ ] `project_issues.templ` — `IssueCommentsFragment`, `IssueAttachmentsStrip` (renders inline in body for screenshots); reuse `humanBytes` from projects.templ
5. [ ] Reuse `projects.js` MutationObserver + WeakSet pattern for issue-attachment uploader

Verification: member opens an issue → drags a screenshot → tab B sees it. Comments thread with edit-grace works identically to project comments.

### Phase 3 — Guest share-link mint + landing + session — status: open

Goal: admin mints a `24h` token, opens the URL in incognito, picks a display name, lands on the project page as a guest. Guest sees the project (read-only) and the issues panel (with "Open new issue" button).

1. [ ] `issues_repo.go` (or new `guest_repo.go` file) — `CreateGuestInvite`, `RevokeGuestInvite`, `GuestInviteByToken`, `ActiveGuestInviteForProject`
2. [ ] `guest.go` — `Identity` resolver (caller-style helper), session keys `project_guest_id` / `project_guest_name` / `project_guest_project_id`, TTL parser (`1h` / `24h` / `7d` / `0` for no-expire)
3. [ ] `issues_service.go` — `MintGuestInvite(projectID, ttl)` revokes the prior active token + inserts new
4. [ ] Public routes `GET /projects/share/{token}` + `POST /projects/share/{token}/join` (landing page + display-name form, sets session, redirects)
5. [ ] Admin actions on project page: "Share with a guest" panel — TTL select + `Mint` / `Revoke` buttons; only renders for project creator + admin
6. [ ] Read-gating pass: every project-area handler that needs guest support uses the package-level `caller(r, projectID)` to resolve identity; templ branches on `IsGuest` to hide member-only affordances

Verification: admin mints link → opens incognito → picks name → lands on project page → no Edit / archive / delete / "add todo" / "post project comment" affordances visible → issues panel still shows the "Open new issue" button.

### Phase 4 — Guest write-path on issues — status: open

Goal: guest creates issues, comments on existing issues, attaches images. All read paths already work after Phase 3.

1. [ ] `caller()` returns guest identity for issue routes; templ renders guest's display name on their posts
2. [ ] `issues_service.CreateIssue` accepts guest creator (`creator_user_id` NULL, `creator_guest_id` set, `creator_name` snapshot)
3. [ ] `issues_service.AddIssueComment` accepts guest author
4. [ ] `issues_service.AddIssueAttachment` accepts guest uploader — synthesises `ownerID = "guest:"+guestID` for the uploads row
5. [ ] Guests cannot change status / delete issue (handler 403s if `Identity.IsGuest`)
6. [ ] Guests CAN edit / delete their own comments within the grace window
7. [ ] Project page hides member-only project actions for guests

Verification: guest opens new issue with a screenshot → admin sees it; admin replies → guest sees it. Guest tries `POST /todo` → 403. Guest reloads after revoke → landing says "invalid invite".

### Phase 5 — Spec sync + final polish — status: open

Goal: spec reflects shipped reality; CHANGELOG auto-appended; activity panel optionally widened.

1. [ ] Inline-refine spec (status flips, friction notes, future updates)
2. [ ] Final progress log entry
3. [ ] (Optional) widen `RecentActivity` SQL UNION to include `project_issues.created_at` + `project_issue_comments.created_at`
4. [ ] Merge to main

## Verification

End-to-end story we want green after Phase 5:

- Two browser windows, one admin tab + one incognito guest tab.
- Admin mints a 24h share link, copies it into the guest tab.
- Guest picks display name "Visitor", lands on the project page.
- Guest sees the project header, todos, attachments, comments — all read-only (no edit buttons).
- Guest opens a new issue "Login button broken" with a screenshot.
- Admin sees the issue in the issues panel, status `open`. Status dropdown advances `triaged → in_progress → closed`.
- Both tabs comment back and forth on the issue, attach images, edit + delete (within grace).
- Admin revokes the share token. Guest's next page navigation gets the invalid-invite landing.
- Restart container; persisted data (projects + issues + comments + attachments) survives.

## Adjustments

<!-- Plans evolve. Document changes with timestamps. -->

## Progress Log

<!-- Updated after every completed action. -->
