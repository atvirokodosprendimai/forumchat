---
name: spec-projects-per-community-collaborative
status: draft
type: spec
tldr: Per-community collaborative project pages with title, description, project-local todos, drag-and-drop document attachments, and datastar-driven realtime updates — feature-flagged via .env.
---

# Projects — per-community collaborative pages with docs, todos, comments

## Target

Communities frequently want a shared artifact richer than chat and looser than forum threads — a "project" page with a description, a living checklist, a pile of supporting documents, and inline discussion, all collaboratively editable by every approved member. Today users improvise with chat pins + forum threads + external Google Drive folders; the spec eliminates the round-trip.

The feature is opt-in at the deployment level: a single `.env` flag enables/disables routes, nav, and migration usage across the whole instance.

## Behaviour

### Feature flag

- New env `PROJECTS_ENABLED` (bool, default `false`). Read once at boot via `internal/config`.
- When `false`:
  - `/c/{slug}/projects/*` routes are NOT mounted.
  - The "Projects" nav link is hidden in the topbar viewer (`Viewer.ProjectsEnabled` is false).
  - The Projects migration still runs (table is created) so toggling on later doesn't need a schema migration.
- When `true`: full routes, nav link, and SSE streams are available to every approved community member.

### Index page — `/c/{slug}/projects`

- Lists active projects in `updated_at DESC` order: title, one-line description preview, todo progress pill (`3/8 done`), attachment count, comment count.
- "New project" button at the top: opens an inline form (title required, description optional, markdown). Any approved member can create.
- Archived projects collapsed under an expandable section, sorted by `archived_at DESC`. Archive ≠ delete.

### Project page — `/c/{slug}/projects/{id}`

Five panels driven by one server-rendered struct (`web/templ.ProjectPageView`) and refreshed via SSE:

1. **Header** — title (inline-editable), description (markdown, inline-editable), `archive` and `delete` buttons (visibility per permissions below).
2. **Todos** — project-local checklist. Add row, toggle done, edit text inline, delete. Drag-reorder. No personal-todos coupling.
3. **Attachments** — drag-and-drop zone + "Choose file" button. Any MIME type. Each row: filename, size, uploader, uploaded-at, ⬇ download button, 🗑 delete button (uploader + creator + admin only). The download link streams from `internal/uploads` with the original filename preserved via `Content-Disposition`.
4. **Comments** — datastar SSE chat-style thread, ordered ascending, identical edit-grace / delete model as `internal/forum` posts. New comment textarea pinned to the bottom.
5. **Activity** — collapsed sidebar listing audit events (created / edited / file added / comment added) for the project, scoped to the last 30.

### Permissions

By default a project is **open**: every approved community member can read and
write it (this is the original behaviour and the zero-value of the model below).

- **Create**: any approved community member.
- **Edit** (title, description, todos, comments, add attachments): any approved community member.
- **Delete an attachment**: uploader OR project creator OR community admin.
- **Delete a comment**: author OR community admin (forum edit-grace rules).
- **Delete / archive a project**: project creator OR community admin.
- Pending or non-member users get the standard 403.

#### Opt-in per-project permissions (migration 00063)

A project can be marked **needs-perms** (a checkbox on create, or the manager-only
Permissions panel later). When off, everything above is unchanged. When on:

- `visibility` decides who may **see** the project:
  - `community` — every approved member can read it (e.g. the support project
    where all development happens).
  - `restricted` — only the creator, community admin/owner, and people on the
    per-person ACL can see it; it vanishes from the index, the `/issues`
    picker, and 404s on direct URL (the "hide" switch — e.g. the email-drop
    Inbox, hidden in both SaaS and self-host).
- `member_access` is the community-wide **write** default for `community`
  visibility: `read` (read-only for all; the default) or `write`.
- A per-person **ACL** (`project_members`) grants one user `read` or `write`,
  overriding the community default — so "everyone reads, these few write".

`EffectiveAccess` (one pure function) is the single read/write authority,
resolving: guest → read · open project → read+write · creator/admin/owner →
manage (read+write) · explicit ACL grant → that level · `community` visibility →
`member_access` · `restricted` with no grant → none. Managing perms + the ACL is
creator/admin/owner only (not a write-granted member). Available whenever
`PROJECTS_ENABLED`, independent of `SAAS`. The mailbox Inbox is auto-restricted.

### Realtime — the datastar virtual-DOM pattern

The user described the right mental model: backend holds the canonical struct, templ renders it to an HTML fragment per element, datastar morphs the live DOM.

- One in-memory `ProjectState` per project (loaded lazily, evicted on idle) holds the current state.
- A `projects.Bus` fans out `Event{Kind: "title" | "desc" | "todo" | "attachment" | "comment" | "archive"}` to every subscriber of `/c/{slug}/projects/{id}/stream`.
- The SSE handler patches **only the affected fragment** (`#proj-header`, `#proj-todos`, `#proj-attachments`, `#proj-comments`, `#proj-activity`) with `sse.PatchElementTempl(..., WithModeOuter())`.
- All POST mutators (`/title`, `/desc`, `/todo`, `/attachment/upload`, `/comment`, `/archive`, etc.) update the persisted row + in-memory state + publish a Bus Event, then return 204. Re-render flows out of the SSE.
- Optimistic UI is NOT used — datastar's morph is fast enough at fragment granularity. Match the forum pattern.

### Feature flag check at every layer

- Routes: skip mount if disabled.
- Templ Viewer struct: `ProjectsEnabled bool`; nav templ checks it.
- Handler: every entrypoint also checks `cfg.ProjectsEnabled` to defend against route-mount drift.
- Migration: always runs (cheap, idempotent).

## Schema

```sql
CREATE TABLE projects (
    id              TEXT PRIMARY KEY,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    creator_user_id TEXT NOT NULL REFERENCES users(id),
    title           TEXT NOT NULL,
    description_md  TEXT NOT NULL DEFAULT '',
    description_html TEXT NOT NULL DEFAULT '',
    archived_at     INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_projects_community ON projects(community_id, updated_at DESC);

CREATE TABLE project_todos (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    body         TEXT NOT NULL,
    done         INTEGER NOT NULL DEFAULT 0,
    sort_order   INTEGER NOT NULL,
    created_by   TEXT NOT NULL REFERENCES users(id),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL
);
CREATE INDEX idx_project_todos_project ON project_todos(project_id, sort_order);

CREATE TABLE project_attachments (
    id            TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    upload_id     TEXT NOT NULL REFERENCES uploads(id) ON DELETE CASCADE,
    filename      TEXT NOT NULL,
    mime          TEXT NOT NULL,
    size_bytes    INTEGER NOT NULL,
    uploader_id   TEXT NOT NULL REFERENCES users(id),
    created_at    INTEGER NOT NULL
);
CREATE INDEX idx_project_attachments_project ON project_attachments(project_id, created_at DESC);

CREATE TABLE project_comments (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    author_id    TEXT NOT NULL REFERENCES users(id),
    body_md      TEXT NOT NULL,
    body_html    TEXT NOT NULL,
    edited_at    INTEGER,
    deleted_at   INTEGER,
    created_at   INTEGER NOT NULL
);
CREATE INDEX idx_project_comments_project ON project_comments(project_id, created_at);
```

Attachments piggyback on the existing `internal/uploads` table; we never duplicate file bytes. Per-file size limit is the existing `UPLOADS_MAX_BYTES`. No per-project file cap.

Permissions (migration 00063) extend `projects` with defaulted columns so
existing rows are byte-for-byte unchanged, plus a per-person ACL table:

```sql
ALTER TABLE projects ADD COLUMN needs_perms   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE projects ADD COLUMN visibility    TEXT NOT NULL DEFAULT 'community'; -- community | restricted
ALTER TABLE projects ADD COLUMN member_access TEXT NOT NULL DEFAULT 'read';      -- read | write (community default)

CREATE TABLE project_members (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    access     TEXT NOT NULL DEFAULT 'read',  -- read | write
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX idx_project_members_user ON project_members(user_id);
```

## Design

### Package layout

```
internal/projects/
  bus.go          — Event fan-out (per-project subscriber set, same shape as rooms.Bus)
  handler.go      — HTTP handlers + SSE stream
  repo.go         — SQL queries
  service.go      — write-then-publish glue
  state.go        — in-memory per-project Snapshot cache (optional first iteration; can hit DB)
  types.go        — Project, Todo, Attachment, Comment, Event
web/templ/
  projects.templ  — ProjectsGrid, ProjectPage, project-*-fragment templs
web/static/
  projects.js     — drag-drop attachment uploader, optimistic file-row insertion
```

### Routing

Under the existing `r.Route("/c/{slug}")` group:

```
GET    /projects                       index
POST   /projects                       create
GET    /projects/{id}                  page
GET    /projects/{id}/stream           SSE (presence + fragment pushes)
POST   /projects/{id}/title            edit title
POST   /projects/{id}/desc             edit description (markdown)
POST   /projects/{id}/archive          toggle archive
POST   /projects/{id}/delete           hard delete (creator + admin)
POST   /projects/{id}/todo             add todo
POST   /projects/{id}/todo/{tid}       edit body
POST   /projects/{id}/todo/{tid}/toggle toggle done
POST   /projects/{id}/todo/{tid}/sort  drag-reorder (body: {after_id})
POST   /projects/{id}/todo/{tid}/delete delete
POST   /projects/{id}/attachment       multipart upload (one or many files)
GET    /projects/{id}/attachment/{aid}/download stream + Content-Disposition
POST   /projects/{id}/attachment/{aid}/delete   delete
POST   /projects/{id}/comment          post comment
POST   /projects/{id}/comment/{cid}    edit (within forum edit-grace)
POST   /projects/{id}/comment/{cid}/delete delete
```

`{id}` UUID — no colon, so no chi URL-decoding trap like `internal/rooms` had.

### Render fragments

Mirror the forum pattern: each templ writes a top-level element with a stable id (e.g. `<section id="proj-todos">…</section>`) and the SSE patches use `WithModeOuter()` so morphdom swaps the subtree in place. The page templ embeds five such fragments inside a layout.

## Verification

- Toggling `PROJECTS_ENABLED=false` in `.env` removes the nav link and 404s `/c/main/projects` after restart.
- Two browser tabs as different members: tab A adds a todo → tab B's checklist updates within 1s without a refresh.
- Drag a PDF onto the attachments zone → row appears with original filename → click ⬇ → browser downloads the file with the original name (not a UUID).
- Tab A toggles a checkbox; tab B sees the strike-through immediately.
- Non-creator non-admin sees no delete button on someone else's attachment.
- Archive a project; index page moves it under "Archived" without losing data.
- Restart container; previous projects + attachments + todos still load.

## Friction

- `internal/uploads` currently signs URLs for chat-embedded images; project download uses a different code path (forced attachment download). We'll add an `UploadStream(w, r, id, asAttachment bool)` helper rather than reusing the inline-image path.
- Drag-reorder of todos requires either a fractional `sort_order` float or a renumber-on-insert. First cut renumbers on insert — N is small per project (<100 items).
- Comments reuse the markdown sanitizer (`internal/render`) and edit grace window (15min by default). Reusing forum's `editGrace` config — no new env.
- No notifications. A user has to open the project (or be subscribed to the SSE stream) to see updates. Acceptable for v1.

## Interactions

- Depends on [[spec - forumchat - community web app with realtime chat and forum threads]] (community membership, SSE patterns, datastar idioms).
- Depends on `internal/uploads` for file storage and the existing `UPLOADS_MAX_BYTES` cap.
- Adjacent to [[spec - todos - personal-todos-from-chat-and-forum]] but does NOT share storage — project todos are independent.
- Does NOT depend on `internal/rooms` even though the SSE fan-out pattern is the same.

## Mapping

> [[internal/projects/handler.go]]
> [[internal/projects/service.go]]
> [[internal/projects/repo.go]]
> [[internal/projects/bus.go]]
> [[internal/projects/types.go]]
> [[web/templ/projects.templ]]
> [[web/static/projects.js]]
> [[internal/storage/sqlite/migrations/0001N_projects.sql]]
> [[internal/config/config.go]] (new ProjectsEnabled field)
> [[cmd/app/main.go]] (mount under feature flag)

## Future

- {[x] per-project access control — restrict to a subset of members (migration 00063, 2026-06-24): needs_perms + visibility + member_access + project_members ACL; see Permissions above}
- {[?] invite OUTSIDE collaborators (non-members) to a single project — distinct from the member ACL; the share-link guest path is the closest existing primitive}
- {[?] notifications on @-mentions inside description or comments}
- {[?] file versioning — keep the previous upload when a same-named file is re-uploaded}
- {[?] export project as PDF (description + todo checklist + comments)}
- {[!] reuse for the upcoming "knowledge base" idea — same panel layout, different defaults}

## Notes

- Feature flag naming: prefix with `PROJECTS_` (not `ROOMS_PROJECTS_`) — it's an instance-level toggle, not a per-feature subsetting.
- The user's "datastar virtual DOM" mental model matches what we already do in `internal/forum` and `internal/rooms`. This spec just formalises it as the explicit pattern: state struct → templ fragment → SSE PatchElementTempl(WithModeOuter()).
- Effective Go: keep packages small, types in `types.go`, no util packages. One `Service` with explicit dependencies. Errors are sentinel values declared at the top of `service.go`.
