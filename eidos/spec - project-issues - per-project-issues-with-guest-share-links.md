---
name: spec-project-issues-with-guest-share-links
status: shipped
type: spec
tldr: Issues sub-resource on projects (title, body, status, image attachments, comments) plus per-project guest share links — a TTL-scoped token grants outsiders read-only access to the project surface and write access on issues only.
---

# Project Issues — per-project tickets with guest share links

## Target

Two related extensions to the Projects feature:

1. **Issues** as a structured way to track work or problems against a project — lighter than a forum thread, more durable than a chat message. Each project gets its own list of issues with an explicit lifecycle (open → triaged → in-progress → closed).
2. **Guest share links** so a project owner can hand a client a single URL good for a fixed window (default 24h) that lets the client read the project, see the issues, and open new issues (with image attachments) without registering for a community.

The two land together because most of the "give the client read access" requests are really "let them file issues" requests in disguise.

## Behaviour

### Page layout — MPA with tabs

Per the project owner's latest decision, the project page becomes a
multi-page app rather than the one-page-many-panels layout shipped
in the parent Projects spec. Each tab is its own URL with its own
server-rendered HTML page; the realtime SSE pattern stays but lives
inside one tab at a time.

- `/c/{slug}/projects/{id}` — Overview tab (default). Title +
  description (markdown), counts of todos / files / issues / comments,
  and the share-link admin section. No body lists.
- `/c/{slug}/projects/{id}/todos` — Todos tab.
- `/c/{slug}/projects/{id}/docs` — Attachments tab (renamed from
  "attachments" to "docs" for the tab label; the route name and
  database table stay `attachments`).
- `/c/{slug}/projects/{id}/issues` — Issues list tab.
- `/c/{slug}/projects/{id}/issues/{iid}` — single issue view.
- `/c/{slug}/projects/{id}/comments` — Project-wide comment thread
  (the original `#proj-comments` panel, now a tab).
- `/c/{slug}/projects/{id}/activity` — Activity log tab.

A persistent tab strip renders at the top of every project sub-page
with the project title + the seven links above. The `aria-current`
attribute on the active tab is set so CSS can highlight it.

Per-tab SSE: each tab opens its own per-project event stream filtered
to its concern (header / todos / attachments / comments / issues /
activity). The stream URL stays `/projects/{id}/stream` — the
handler renders only the fragments needed by the current tab based on
a query param (`?tab=todos`, etc.) so we don't morph fragments that
aren't on the page.

### Issues — model and lifecycle

- One issue carries: `title` (≤120 chars), `body_md` (rendered to `body_html`), `status`, `creator_user_id` OR `creator_guest_id`, `creator_name` (display snapshot — guests don't persist in `users`).
- Statuses: `open`, `triaged`, `in_progress`, `closed`. Linear advancement isn't enforced — any member can move an issue to any status. `closed` issues collapse under an expandable section similar to archived projects.
- Auth members and guests can create issues. Only auth members can change status (guests don't get the status UI).
- Issues live at `/c/{slug}/projects/{id}/issues/{iid}` (a UUID). The project page gains a "Issues (3 open)" panel that lists them with status pill + title; click to drill in.

### Issue comments

- Every issue gets its own forum-style comment thread with edit-grace + soft-delete.
- Both auth members and guests may post comments on any issue (per the user decision; not just on issues they themselves opened). Guests inherit the guest display name from their session.
- Edit-grace: 15min by default. Author + admin can edit/delete inside the window; only admin can edit/delete outside.

### Issue attachments

- Issue body has an inline drag-drop or paste-image affordance (mirrors the chat/forum paste-image flow).
- Issue comments also support a "Attach image" button.
- Image types only on issues (`image/jpeg`, `png`, `gif`, `webp`) — reuses `uploads.Store.Save` (the existing image-whitelist path), NOT `SaveAttachment`. Documents on issues are a future extension.
- Storage row + signed-URL flow are identical to chat-image attachments.

### Guest share links

- New table `project_guest_invites`: `token PRIMARY KEY`, `project_id REFERENCES projects(id)`, `created_by REFERENCES users(id)`, `expires_at INTEGER NULL`, `revoked_at INTEGER NULL`, `created_at INTEGER`.
- Admin / project creator mints a link from the project page admin section. Form picks one of: `1h | 24h | 7d | no-expire`. Only one active token per project at a time — minting a fresh one revokes the previous (same rule as rooms invites).
- Landing URL `/projects/share/{token}` (public root, no community context required — the token resolves the project which resolves the community).
- Landing page asks for display name, then `POST /projects/share/{token}/join` sets session keys:
  - `project_guest_id` (UUID minted on join)
  - `project_guest_name`
  - `project_guest_project_id` (constrains the guest to one project)
- Guest is then redirected to `/c/{slug}/projects/{id}`.

### Identity resolution — `caller()`

A new package-level helper resolves three identity sources, in priority order, on every project-area request:

1. Auth user (existing `auth.FromContext`) → full member privileges per role.
2. Guest session keys present AND `project_guest_project_id == roomID URL param` → guest identity, restricted action set.
3. Neither → 401 (or redirect to the share-link landing if a token was provided).

Pattern mirrors `internal/rooms/handler.caller()` — including the lesson learned about `chi.URLParam` decoding for project IDs (project IDs are UUIDs without reserved chars, so no decode trap, but document at the helper).

### Permission matrix

| Action                                | Auth member | Guest |
|---------------------------------------|:----------:|:-----:|
| Read project (header, todos, attach.) | ✓          | ✓     |
| Read project comments / activity      | ✓          | ✓     |
| Edit project title / description      | ✓          | ✗     |
| Add / edit / toggle / delete todo     | ✓          | ✗     |
| Add / delete project attachment       | ✓ (rules)  | ✗     |
| Post / edit / delete project comment  | ✓ (rules)  | ✗     |
| Archive / delete project              | creator+admin | ✗ |
| Read issue list / issue body          | ✓          | ✓     |
| Open new issue                        | ✓          | ✓     |
| Comment on any issue                  | ✓          | ✓     |
| Change issue status                   | ✓          | ✗     |
| Delete issue                          | author + admin | own only (within grace) |
| Mint / revoke guest share link        | creator+admin | ✗ |

### Routes (added to `/c/{slug}/projects/{id}` under the feature flag)

In addition to the MPA tab routes listed above, the new issue-area
routes are:

```
GET    /issues                                list
POST   /issues                                create  (guest-OK)
GET    /issues/{iid}                          view
GET    /issues/{iid}/stream                   SSE per-issue
POST   /issues/{iid}/status                   member-only (open|triaged|in_progress|closed)
POST   /issues/{iid}/delete                   author/admin
POST   /issues/{iid}/comment                  add (guest-OK)
POST   /issues/{iid}/comment/{cid}            edit (author within grace OR admin)
POST   /issues/{iid}/comment/{cid}/delete     author OR admin
POST   /issues/{iid}/attachment               multipart image upload (guest-OK)
POST   /share                                 mint a fresh guest token (admin)
POST   /share/revoke                          revoke active token (admin)
```

Public-root routes for the guest-join flow:

```
GET    /projects/share/{token}                landing page
POST   /projects/share/{token}/join           sets session + redirect
```

### Feature flag

- Reuse the existing `PROJECTS_ENABLED` flag — issues + guest sharing live or die with Projects.
- No new env var.

### Realtime

- Per-issue SSE stream + Bus event kinds `issue` (title/status/body) and `comments` and `attachments` — identical pattern to project-level events but scoped per-issue.
- Project page's issues panel re-renders on a new "issues" event published by issue CRUD, so opening a new issue updates the list across tabs.

## Schema

```sql
CREATE TABLE project_issues (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title           TEXT NOT NULL,
    body_md         TEXT NOT NULL DEFAULT '',
    body_html       TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open','triaged','in_progress','closed')),
    creator_user_id  TEXT,  -- NULL for guest
    creator_guest_id TEXT,  -- NULL for auth user
    creator_name    TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_project_issues_project_status ON project_issues(project_id, status, updated_at DESC);

CREATE TABLE project_issue_comments (
    id              TEXT PRIMARY KEY,
    issue_id        TEXT NOT NULL REFERENCES project_issues(id) ON DELETE CASCADE,
    author_user_id  TEXT,  -- NULL for guest
    author_guest_id TEXT,
    author_name     TEXT NOT NULL,
    body_md         TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    edited_at       INTEGER,
    deleted_at      INTEGER,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_project_issue_comments_issue ON project_issue_comments(issue_id, created_at);

CREATE TABLE project_issue_attachments (
    id           TEXT PRIMARY KEY,
    issue_id     TEXT NOT NULL REFERENCES project_issues(id) ON DELETE CASCADE,
    comment_id   TEXT REFERENCES project_issue_comments(id) ON DELETE CASCADE,
    upload_id    TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
    uploader_user_id  TEXT,
    uploader_guest_id TEXT,
    uploader_name TEXT NOT NULL,
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_project_issue_attachments_issue ON project_issue_attachments(issue_id, created_at);

CREATE TABLE project_guest_invites (
    token       TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_by  TEXT NOT NULL REFERENCES users(id),
    expires_at  INTEGER,
    revoked_at  INTEGER,
    created_at  INTEGER NOT NULL
);
CREATE INDEX idx_project_guest_invites_project ON project_guest_invites(project_id);
```

## Design

### Package layout addition

```
internal/projects/
  issues.go          — Issue/IssueComment/IssueAttachment types + Bus event kind
  issues_repo.go     — issue + comment + attachment queries (separate file so handler.go stays under 1k LoC)
  issues_service.go  — CRUD + status transition + permission checks
  issues_handler.go  — per-issue routes + stream + guest middleware
  guest.go           — share-link token mint/revoke + landing + session shape + Identity resolver
web/templ/
  project_issues.templ  — IssuesList + IssuePage + IssueStatusPill fragments
```

### Identity

```go
// internal/projects/guest.go
type Identity struct {
    UserID  string
    GuestID string
    Name    string
    Role    auth.Role  // RoleMember for guest (auth.Loader fills auth users)
}
func (id Identity) IsGuest() bool { return id.UserID == "" && id.GuestID != "" }
func (id Identity) Key() string {
    if id.UserID != "" { return "u:" + id.UserID }
    return "g:" + id.GuestID
}
```

A package-level `caller(r, projectID)` mirrors `rooms.Handler.caller()`. Guests are constrained to a single project via the session's `project_guest_project_id` key.

### Routing

```go
// auth-only project routes (already exists)
r.Group(func(r chi.Router) {
    r.Use(auth.RequireAuth)
    r.Use(community.RequireMember(...))
    // existing /projects/* mounts
})

// open project routes (auth member OR guest)
r.Group(func(r chi.Router) {
    r.Use(community.LoadCommunity(cRepo))
    // /c/{slug}/projects/{id}/issues/...
    // /c/{slug}/projects/{id}/share/...
    // guest is resolved inside the handler via caller()
})
```

Public guest-landing routes are mounted at root (`/projects/share/{token}` and `/projects/share/{token}/join`), no community context — token resolves the project which resolves the community.

### SSE fragment pattern

Each issue page has its own SSE stream `/issues/{iid}/stream`. The Bus is per-issue (keyed by issue ID, not project ID) so opening an issue doesn't get noise from sibling issues. The issues-LIST panel on the project page subscribes to a "issues" event scoped to the project, published when issues are created / status-changed / deleted.

## Verification

- Member creates an issue from the project page → issue lands at `/projects/{id}/issues/{iid}` with status pill `open`.
- Member changes status `open → triaged → in_progress → closed` → list panel re-renders the pill in both tabs.
- Member attaches a screenshot to an issue body → image renders inline.
- Admin mints a guest share link with `24h` TTL → copies URL → opens in incognito → picks name → lands on the project page.
- Guest sees read-only header / todos / attachments / project-comments (no edit affordances visible) — but the issues panel shows a "Open new issue" button.
- Guest opens a new issue with a screenshot → both tabs (admin + guest) see the new issue immediately.
- Guest comments on any other issue → admin sees it; admin replies → guest sees it.
- Guest tries POST `/projects/{id}/todo` → 403.
- Admin revokes the token → guest's next action 401s, landing page now says "invalid invite".
- Token TTL expires while guest tab is open → next POST 401s; reading still works until next page navigation (acceptable for v1).

## Friction

- Guests are scoped per-project, not per-community: the same guest URL can't drift into other projects. Trade-off: a client wanting access to two projects needs two links. Probably fine.
- `uploads.Save` requires `ownerID` and `communityID`. For guest uploads we'll synthesise: ownerID = synthetic prefixed `"guest:"+guestID`, communityID = the project's. The uploads table already has those columns and no FK to users — fine.
- Status edits are auth-only by design. If a guest reports an issue that needs triage, they have to wait for an auth member to advance it. Avoids guests closing their own complaints.
- Activity panel on the project page does NOT currently include issue events. Phase 4 will widen the SQL UNION (or skip and document).
- The CHANGELOG auto-append will fire on every phase commit. Fine.

## Interactions

- Depends on [[spec - projects - per-community-collaborative-projects]] — issues are a sub-resource of a project.
- Depends on `internal/uploads` for image storage.
- Depends on `internal/auth` for member identity AND for the guest session-store backing.
- Reuses the chi URL-decode pattern lesson from `internal/rooms/handler.go:roomIDParam` — apply early so we never regress.

## Mapping

> [[internal/projects/issues.go]]
> [[internal/projects/issues_repo.go]]
> [[internal/projects/issues_service.go]]
> [[internal/projects/issues_handler.go]]
> [[internal/projects/guest.go]]
> [[web/templ/project_issues.templ]]
> [[internal/storage/sqlite/migrations/0001N_project_issues.sql]]
> [[cmd/app/main.go]] (route mounts under PROJECTS_ENABLED)

## Future

- {[?] guest can attach DOCUMENTS not just images on issues — would need uploads.SaveAttachment + a per-issue MIME whitelist}
- {[?] email notifications to the project creator when a guest opens an issue}
- {[?] issue assignee + labels + due-date (closer to GitHub Issues)}
- {[?] guest tokens scoped to MULTIPLE projects so an agency can share one link covering a whole engagement}
- {[?] read-only "guest viewer" mode on the project surfaces — Phase 4 only does read-gating, but a "presented to a client" mode that hides admin chrome would polish the experience}
- {[!] activity panel widening to include issue events}

## Notes

- Status transitions are linear by convention but not enforced; the UI can render a step-progress visualisation without backend changes.
- Guest sessions reuse the existing scs session manager — no new cookie name, no new store.
- The "comment on any issue" decision (per user) means we won't add per-issue subscription tracking; everyone with access can comment on everything.
