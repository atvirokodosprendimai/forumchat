---
tldr: Build per-project Issues sub-resource and guest share-link sharing in 6 phases. Visible result at the end of each phase. Reuses the Projects spec's datastar virtual-DOM pattern. Guest write scope = create issues + comment on any issue + attach images.
status: completed
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

### Phase 1 — Issues data model + list + create (member-only) — status: completed

Goal: any approved community member can open a new issue from the project page, see it land in the issues list, and click into the issue page. Status defaults to `open`. No comments, no attachments, no guests yet.

1. [x] Migration `00014_project_issues.sql` — all four tables ship together (project_issues, project_issue_comments, project_issue_attachments, project_guest_invites)
2. [x] `internal/projects/issues.go` — `Issue`, `IssueComment`, `IssueAttachment`, `GuestInvite`, `Identity` (used everywhere), `IssueEvent` kinds, IssueStatuses constants
3. [x] `internal/projects/issues_repo.go` — ListIssues, IssueByID, InsertIssue, UpdateIssueTitle/Body/Status, DeleteIssue, CountOpenIssues
4. [x] `internal/projects/issues_service.go` — CreateIssue, UpdateIssueStatus (guest-blocked via ErrForbidden), UpdateIssueTitle/Body (author+admin), DeleteIssue (author+admin). All publish `Event{Kind:"issues"}` so the issues tab re-renders.
5. [x] `internal/projects/issues_handler.go` — GetIssuesTab, GetIssue, PostCreateIssue, PostIssueEdit, PostIssueStatus, PostIssueDelete; callerIdentity helper (Phase 3 extends to guests)
6. [x] Templ — `ProjectIssuesPage` (list + new-issue form), `ProjectIssuePage` (header + body + status dropdown + edit/delete), `IssueStatusPill`, `issueLabel` for display strings
7. [x] CSS — issue pills (4 colour-coded states), issue list rows, issue page head, edit form
8. [x] main.go — 6 new routes wired under PROJECTS_ENABLED guard

Verification: member opens project page → clicks "Open new issue" → fills title + body → lands on `/projects/{id}/issues/{iid}` with status pill `open`. Status dropdown advances to `triaged`/`in_progress`/`closed`. Closed issues collapse under a `<details>`.

### Phase 2 — Issue comments + image attachments — status: completed

Goal: members can comment on issues and attach inline images.

1. [x] `issues_comments_repo.go` — ListIssueComments, IssueCommentByID, InsertIssueComment, UpdateIssueComment, SoftDeleteIssueComment, ListIssueAttachments, IssueAttachmentByID, InsertIssueAttachment, DeleteIssueAttachment
2. [x] `issues_service.go` — AddIssueComment, UpdateIssueComment (edit-grace), DeleteIssueComment, AddIssueAttachment (uploads.Store.Save image whitelist; guest ownerID = "guest:" prefix), DeleteIssueAttachment with permission check
3. [x] `issues_handler.go` — PostIssueComment, PostIssueCommentEdit, PostIssueCommentDelete, PostIssueAttachmentUpload (multipart), PostIssueAttachmentDelete; GetIssue now loads comments + body+comment attachments
4. [x] Templ extended — comment list with inline edit/delete, attachment dropzone, image gallery rendered via `issueImage` partial, comment-scoped attachments grouped under each comment
5. [x] `projects.js` — `bindIssueZone` handler with the same WeakSet guard pattern; MutationObserver on `.project-panel-issue` re-binds after SSE morphs
6. [x] CSS — image figures with delete overlay, dropzone, comment thread
7. [x] main.go — 5 new routes wired (comment add/edit/delete + attachment upload/delete)
8. [p] Per-issue SSE stream postponed — project-wide SSE already covers issue updates via `Event{Kind:"issues"}` since the entire issues page re-renders. Per-issue stream would only matter for very large comment threads; defer.

Verification: member opens an issue → drags a screenshot → tab B sees it. Comments thread with edit-grace works identically to project comments.

### Phase 3 — Guest share-link mint + landing + read-gating — status: completed

Goal: admin mints a `24h` token, opens the URL in incognito, picks a display name, lands on the project page as a guest. Guest sees the project (read-only) and the issues panel (with "Open new issue" button).

1. [x] `issues_repo.go` — ActiveGuestInviteForProject, GuestInviteByToken, CreateGuestInvite, RevokeGuestInvite
2. [x] `guest.go` — session keys constants, ParseGuestTTL (`1h`/`24h`/`7d`/`0`), MintGuestInvite, RevokeActiveGuestInvite, RedeemGuestInvite, randToken + newGuestID helpers
3. [x] `issues_handler.go` — PostShareMint, PostShareRevoke, GetGuestLanding, PostGuestJoin, GetGuestBounce; callerIdentity now also resolves guest sessions (with project-ID scope check)
4. [x] `ProjectGuestLandingPage` templ + `projectSharePanel` rendered inside ProjectOverviewPage (creator+admin only)
5. [x] `handler.Handler` now has Sessions + commLookup deps; main.go injects via SetCommunityLookup
6. [x] Route restructure in main.go: project OPEN routes (tab GETs + issue write-paths) moved to a new `r.Route("/c/{slug}/projects")` block OUTSIDE the auth-required community group, with only LoadCommunity middleware. Member-only routes (edit/lifecycle/share-mint/issue-status) stay inside the auth group.
7. [x] Read-gating: loadProjectData calls callerIdentity (instead of auth.FromContext), so guest sessions flow through cleanly. `Project.CanDelete` is false for guests; `Share.Visible` is false for guests. Templ hides member-only affordances via these booleans.

Verification: admin mints link → opens incognito → picks name → lands on project page → no Edit / archive / delete / "add todo" / "post project comment" affordances visible → issues panel still shows the "Open new issue" button.

### Phase 4 — Guest write-path on issues — status: completed

Goal: guest creates issues, comments on existing issues, attaches images. All read paths already work after Phase 3.

1. [x] `callerIdentity` returns guest identity for issue routes (already from P3) — services accept guest Identity
2. [x] `CreateIssue` / `AddIssueComment` / `AddIssueAttachment` already accepted Identity in P1+P2 — guest fields propagate to `creator_guest_id`/`author_guest_id`/`uploader_guest_id` columns. Templ shows the "guest" pill on guest-authored rows.
3. [x] `AddIssueAttachment` synthesises `ownerID = "guest:"+guestID` for uploads — P2 already in place.
4. [x] `UpdateIssueStatus` rejects guest via `ErrForbidden` (already P1). Templ hides the status dropdown when `!CanChangeStatus`.
5. [x] Guests CAN edit / delete their own comments within grace — `UpdateIssueComment` permission rule covers it.
6. [x] Project page hides member-only project actions for guests — added `isGuest` param to ProjectHeaderFragment / ProjectTodosFragment / ProjectAttachmentsFragment / ProjectCommentsFragment; passed true when the viewer is a share-link guest. SSE push call sites pass false (member-only stream).
7. [x] Per-fragment branches hide Edit/Add/Delete buttons + write forms; replaced with a "Read-only — open an issue if you'd like to suggest a change." muted hint.

Verification: guest opens new issue with a screenshot → admin sees it; admin replies → guest sees it. Guest tries `POST /todo` → 403. Guest reloads after revoke → landing says "invalid invite".

### Phase 5 — Spec sync + final polish — status: completed

Goal: spec reflects shipped reality; CHANGELOG auto-appended; activity panel optionally widened.

1. [x] Spec status: draft → shipped
2. [x] Plan status: active → completed; final progress log entry below
3. [p] Activity-panel widening to include issues — deferred. The activity panel currently shows project events; issues live inside their own tab, so the omission is acceptable for v1. A future PR can add `project_issues.created_at` + `project_issue_comments.created_at` rows to the UNION.
4. [x] Merge to main as one tagged step

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

- **2026-06-14 15:50** — P0 (MPA tabs refactor) shipped: split ProjectPage into ProjectOverviewPage/ProjectTodosPage/ProjectDocsPage/ProjectCommentsPage/ProjectActivityPage, persistent tab strip, loadProjectData(wants) helper. Commit `a1c8094`.
- **2026-06-14 16:05** — P1 (Issues CRUD member-only) shipped: migration 00014, types, repo, service, handler, templ for issues list + page + status pill. Member-only routes. Commit `0802dab`.
- **2026-06-14 16:20** — P2 (Issue comments + image attachments) shipped: issues_comments_repo.go, AddIssueComment/UpdateIssueComment/DeleteIssueComment, AddIssueAttachment using uploads.Store.Save, ProjectIssuePage extended with image gallery + dropzone + comment thread. Commit `437834c`.
- **2026-06-14 16:38** — P3 (Guest share-link mint + landing + read-gating) shipped: 4 guest repo methods + guest.go + 5 guest handlers, ProjectGuestLandingPage + projectSharePanel templ, Handler.Sessions + commLookup deps, main.go route restructure putting project OPEN routes in a separate r.Route("/c/{slug}/projects") block. Commit `facd6b1`.
- **2026-06-14 16:48** — P4 (Guest write-paths) shipped: most was unlocked by P3's route move + callerIdentity guest support. Templ-side isGuest gating added to ProjectHeaderFragment / TodosFragment / AttachmentsFragment / CommentsFragment. Commit `509b9be`.
- **2026-06-14 16:55** — P5 (spec sync) shipped: spec status -> shipped, plan status -> completed.
