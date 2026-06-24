---
name: plan-project-permissions
status: active
type: plan
created: 2026-06-24
tldr: Opt-in per-project permissions — a needs_perms flag turns on read/write gating with community-wide default access (read|write) + per-person ACL (read|write) + visibility (community|restricted, the "hide" switch). Back-compat: needs_perms=0 means today's behaviour, untouched.
---

# Plan — per-project read/write permissions + visibility (hide)

Implements the spec Future item *"per-project access control — restrict to a
subset of members"* in
[[spec - projects - per-community-collaborative-projects]].

## Context

- Today (`internal/projects`): every approved member can read AND write every
  project; delete/archive is creator/admin; share-link guests are read-only and
  scoped to one project. `loadProjectData` (`handler.go:170`) is the single
  **read chokepoint** — every tab (overview/todos/docs/comments/activity/issues/
  discussions + the issue & thread pages) funnels through it. Write handlers sit
  in the member-only route group (`main.go:1951`) with **no** per-project gate.
- `callerIdentity` (`issues_handler.go:35`) already re-resolves the caller's role
  against the URL-slug community (cross-tenant escalation fix) and yields an
  `Identity{UserID, GuestID, Name, Role}`. All gates ride on this.
- `EnsureNamedProject` (`service.go:77`) seeds the mailbox "Inbox" project
  (`mailbox/service.go:465`) where emails drop, and the support inbox
  (`support/handler.go:111`).
- `ProjectsForCommunities` (`issues_repo.go:267`) powers the cross-community
  `/issues` page — must not leak restricted project titles/counts.
- `memberOptions` (`handler.go:1175`) already builds the community roster for the
  todo-assignee picker — reuse for the ACL editor.

### Decisions (user-confirmed 2026-06-24)

1. **Both modes** — feature is available whenever `PROJECTS_ENABLED`, NOT gated
   by `SAAS`. Hiding a project must work in self-host too. It's a per-project
   property, not tenant config.
2. **Read-only default** — when `needs_perms=1` and `visibility=community`,
   members get READ by default; WRITE requires creator/admin/owner OR an explicit
   per-person `write` grant. To let everyone write, leave `needs_perms=0`.
3. **Auto-hide the email inbox** — the mailbox `Inbox` project becomes
   `restricted` on create/ensure. Support inbox stays open (untouched).

### Model

`projects` gains three columns (all defaulted so existing rows = today):
- `needs_perms INTEGER NOT NULL DEFAULT 0` — master switch.
- `visibility TEXT NOT NULL DEFAULT 'community'` — `community` | `restricted`.
- `member_access TEXT NOT NULL DEFAULT 'read'` — community-wide default when
  visible: `read` | `write` (only meaningful when needs_perms=1 &
  visibility=community).

`project_members(project_id, user_id, access, created_at)` PK `(project_id,
user_id)`, `access ∈ {read, write}` — per-person ACL.

**EffectiveAccess(project, caller, grant, grantOK) → none|read|write** (one pure
function, the single source of truth, reused at every gate):
```
guest (token-admitted to THIS project)      → read
!needs_perms: member→write, (guest→read)    → legacy behaviour, unchanged
creator OR Role.AtLeast(RoleAdmin)          → write   (manage)
explicit grant row                          → grant (read|write)
visibility=community (no grant)             → member_access (read|write)
visibility=restricted (no grant)            → none     (hidden)
```

## Phases

### Phase 0 — Migration + domain types — status: open

1. [ ] `internal/storage/sqlite/migrations/00063_project_permissions.sql` (goose
   up/down): `ALTER TABLE projects ADD COLUMN needs_perms / visibility /
   member_access` with the defaults above; `CREATE TABLE project_members (...)`
   + index. Down drops the table and (best-effort) the columns.
   - => verify: `make build` runs migrations clean on a fresh tmp DB.
2. [ ] `types.go`: extend `Project` with `NeedsPerms bool`, `Visibility string`,
   `MemberAccess string`; add `ProjectMember` struct + `Visibility*`/`Access*`
   const sets + small validators (`ValidVisibility`, `ValidMemberAccess`).

### Phase 1 — Access core (pure, table-tested) — status: open

3. [ ] `internal/projects/access.go`: `AccessLevel` (None/Read/Write) +
   `EffectiveAccess(p Project, caller Identity, grant string, grantOK bool)
   AccessLevel`. No I/O.
4. [ ] `access_test.go`: table test over every branch (legacy on/off, guest,
   creator, admin/owner, community read/write default, restricted with/without
   grant, grant up/downgrade).
   - => verify: `go test ./internal/projects/`.

### Phase 2 — Repo + service wiring — status: open

5. [ ] `repo.go`: scan the 3 new columns in `ByID` + `listForCommunity`; add
   `MemberAccessFor(projectID,userID) (string,bool)`, `ListMembers(projectID)`,
   `SetProjectMember`, `RemoveProjectMember`, `SetPerms(projectID, needsPerms,
   visibility, memberAccess)`. Add `listVisibleForCommunity(communityID, userID,
   isAdmin, archived)` (SQL predicate: `needs_perms=0 OR visibility='community'
   OR creator=? OR EXISTS(project_members) OR isAdmin`) — parametrise the
   existing `listForCommunity`, don't fork it.
6. [ ] `service.go`: `SetPerms` (validates, manage-gate = creator/admin/owner,
   publishes `Event{Kind:"header"}`), `GrantMember`/`RevokeMember` (same gate,
   publish header). `CreateProject` gains the perms params (or an opts struct) so
   the create form can set them atomically. Add `EnsureHiddenProject` wrapping a
   shared internal with restricted=true; `EnsureNamedProject` delegates with
   restricted=false — only `mailbox/service.go` switches to the hidden variant.
   - => verify: `go test ./...`.

### Phase 3 — Read gate + index/global filtering — status: open

7. [ ] `loadProjectData`: resolve `EffectiveAccess`; `AccessNone` → `404`
   (no existence oracle). Set `data.Project.CanWrite` from access≥Write. This one
   change gates EVERY read tab + issue/discussion page.
8. [ ] `GetIndex`: swap to `listVisibleForCommunity` for active + archived so
   hidden projects vanish from the grid for non-members.
9. [ ] `ProjectsForCommunities`: add the same visibility predicate (caller
   userID + admin community set) so `/issues` doesn't leak restricted projects.
   - => verify: build + manual route check.

### Phase 4 — Write gate (middleware) — status: open

10. [ ] `RequireWrite` middleware on `Handler`: resolve community+project+caller+
    grant, enforce access≥Write, else 403 (404 if not even readable). Mount it on
    a sub-group of the member-only block holding ONLY the mutation routes
    (title/desc/todo*/attachment delete+move+upload/comment*); keep `GetIndex`,
    `GetStream`, archive/delete (own creator/admin gate) as-is. Also gate the
    open-group issue/discussion/comment **create+edit** writes for perms projects
    (guests keep their token-admitted issue path).
    - => verify: member with read-only access gets 403 on POST; write-granted
      member succeeds; build green.

### Phase 5 — UI (templ + datastar) — status: open

11. [ ] Create form (`projects.templ` `ProjectsGrid`): a "Restrict access
    (permissions)" checkbox (`data-bind`), revealing visibility + default-access
    selects via `data-show`. `PostCreate` reads them.
12. [ ] Project header: a "Permissions" panel (creator/admin/owner only) —
    needs_perms toggle, visibility + member_access selects, and the per-person
    ACL editor (reuse `memberOptions` roster; add/remove rows with access
    select). Stable-id fragment `#proj-perms`, SSE-morphed via the header event.
    New routes: `POST /{id}/perms`, `POST /{id}/perms/member`,
    `POST /{id}/perms/member/{uid}/delete`. Hide write affordances across all
    fragments when `!CanWrite`.
13. [ ] `make gen`; `make build`; `go test ./...`.
    - => verify: two-browser manual — read-only member sees no edit controls; an
      admin grants write → that member's affordances appear after refresh; a
      restricted project is absent from another member's index and 404s on direct
      URL.

### Phase 6 — Spec + review + memory — status: open

14. [ ] Update [[spec - projects - per-community-collaborative-projects]]:
    move the Future bullet into Behaviour/Schema; document the model.
15. [ ] Codex read-only review of the diff (auth/visibility = security surface),
    fold findings, recommend `/codex:review` to the user.
16. [ ] mempalace diary + a `project_project_permissions.md` memory drawer.

## Verification

- `needs_perms=0` projects behave byte-for-byte as before (regression: existing
  `internal/projects` tests stay green untouched).
- Migration runs clean and is reversible.
- Restricted project: invisible on index + `/issues`, 404 on direct URL for a
  non-granted member; visible to creator/admin/granted members.
- Community-visible read-only project: all members see it, only write-granted
  members + creator/admin can mutate (POST → 403 otherwise).
- Mailbox `Inbox` is restricted after this ships.
- `access_test.go` covers every branch of `EffectiveAccess`.

## Progress log

- 2606241313 — Phases 0–6 implemented. Commits on `task/project-permissions`:
  migration+access core → repo+service → read gate+index/stream → write gate+UI
  → spec. `go test ./...` green throughout; `make build` clean.
- Adjustment: **Phase 3 action 9 dropped** — `ProjectsForCommunities` (global
  `/issues`) needs NO visibility filter; the route is already scoped to
  `AdminCommunityIDs`, and admins see all projects per `EffectiveAccess`, so
  there's no leak.
- Adjustment: discovered + fixed an SSE **fragment leak** not in the original
  plan — `GetStream` only checked community membership; added the shared
  `projectAccess` helper + a `CanRead` gate so a restricted project's fragments
  never stream to a non-granted member.
- UI hiding: replaced the fragments' `isGuest` write-gate param with a
  `readOnly` flag fed by `!CanWrite` (guests already resolve to read-only), and
  threaded `CanWrite` through the push helpers (`viewerCanWrite`) so read-only
  members see no write affordances live, matching initial render.
- Codex read-only review launched on the diff (auth/visibility/migration =
  security surface). Returned 4 HIGH + 2 MEDIUM + 1 LOW; folded the actionable
  ones in (commit `0f738c0`):
  - `MovePeers` + `SearchRefs` leaked restricted project titles → filtered.
  - `GetAttachmentDownload` was community-scope only → now read-gated.
  - `PostIssueMove` / `PostAttachmentMove` didn't check the TARGET project →
    now require write on the target (`callerAccessTo` helper).
  - `GetStream` only checked access at open → now re-checks per event and
    closes the stream on revocation (the actual live SSE leak).
  - ACL grants restricted to current community members (LOW hardening).
  - MEDIUM `EnsureHiddenProject` reusing a user-named "Inbox" left as-is —
    pre-existing find-by-title behaviour; auto-restricting it is acceptable
    (dropped mail becomes private). Noted, not changed.

## Friction / risks

- Write gate spans two route groups (member-only core + open issues/discussions).
  Middleware covers the member-only mutators cleanly; open-group write handlers
  need an inline `access≥Write` check (or a shared helper) so guests keep working.
- Global `/issues` admin-in-other-community may not see a restricted project they
  admin elsewhere — acceptable; reachable via that community's own index. Noted.
- Datastar bool-from-JS gotcha (§4.6): flip the perms checkbox inside the
  datastar expression, not via a hidden input.
