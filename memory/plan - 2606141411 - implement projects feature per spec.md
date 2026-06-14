---
tldr: Build per-community Projects feature in phases that each end with something visible — flag + empty index, then create+view, then realtime, then todos, then attachments, then comments, then polish. Each phase commits separately and the spec stays the source of truth.
status: active
---

# Plan: Implement projects feature per spec

## Context

- Spec: [[spec - projects - per-community-collaborative-projects]] (commit `48b19ee` on `task/spec-projects`)
- Adjacent specs (read-only patterns to mirror, not modify):
  - [[spec - forumchat - community web app with realtime chat and forum threads]] — overall datastar+SSE patterns
  - [[spec - todos - personal-todos-from-chat-and-forum]] — feature-flag UX + nav pattern (per-community toggle there; instance toggle here)
- Existing patterns to reuse:
  - `internal/forum/` — bus, handler, SSE per-thread; closest existing analogue
  - `internal/rooms/` — Bus/Service/State split, fragment-mode SSE patches via `sse.PatchElementTempl(..., WithModeOuter())`
  - `internal/uploads/` — file persistence; will need an attachment-mode download path
  - `internal/config/` — `caarlos0/env` env loader; add `ProjectsEnabled bool`
- Watch out for:
  - chi URLParam URL-encoding trap (see commit `3ef7363` postmortem): project UUIDs are fine but any future composite ID would re-bite. Document at the route level.
  - SCS session pressure under SSE bursts: same constraints as rooms
- Plan-shape decisions:
  - Phase order optimises **visible result early** — Phase 1 ends with the flag working + empty index. Phase 2 ends with a creatable+viewable project (no realtime). Phase 3 adds realtime to the existing surface. Phases 4-6 stack richer panels.
  - Spec stays on `task/spec-projects`; first implementation phase merges spec + Phase 1 to main together so the spec ships alongside its first verifiable bit of code.

## Phases

### Phase 1 — Feature flag, schema, empty index — status: open

Goal: `PROJECTS_ENABLED=true` shows a Projects nav link and a `/c/{slug}/projects` page rendering "No projects yet". `false` hides everything. DB tables exist regardless.

1. [ ] Migration `0001N_projects.sql` — create `projects`, `project_todos`, `project_attachments`, `project_comments` per spec schema
   - Always runs (idempotent CREATE TABLE)
   - Slot number chosen at implementation time (next free)
2. [ ] `internal/config/config.go` — add `ProjectsEnabled bool ` env:`PROJECTS_ENABLED` envDefault:`"false"`
3. [ ] `internal/projects/types.go` — `Project`, `Todo`, `Attachment`, `Comment`, `Event` structs (skeleton, fields per schema)
4. [ ] `internal/projects/repo.go` — `ListActiveForCommunity`, `ListArchivedForCommunity`, `Create`, `ByID` only (others come in later phases)
5. [ ] `internal/projects/handler.go` — `Handler` struct, `GetIndex` only; routes mount only when `cfg.ProjectsEnabled`
6. [ ] `web/templ/projects.templ` — `ProjectsGrid(data)` + `ProjectsGridRow` (title, desc preview, counts; empty state)
7. [ ] Wire mount in `cmd/app/main.go` under the existing `/c/{slug}` community group, gated by `cfg.ProjectsEnabled`
8. [ ] Add `ProjectsEnabled bool` to `webtempl.Viewer`; `layout.templ` shows "Projects" nav link when set
9. [ ] Spec merge: fast-forward `task/spec-projects` into main via this phase's PR so spec + first impl land together

Verification: with `PROJECTS_ENABLED=true`, nav shows "Projects", `/c/main/projects` returns 200 with empty grid. With `=false`, link absent, route 404.

### Phase 2 — Create + view a project (form-driven, no SSE yet) — status: open

Goal: any approved member can create a project, click into it, see all five panel placeholders rendered server-side. No live updates yet.

1. [ ] `internal/projects/service.go` — `Service` struct, `CreateProject(ctx, communityID, creatorID, title, desc) (Project, error)`; markdown render reuses `internal/render.RenderMarkdown`
2. [ ] `handler.PostCreate` — POST `/c/{slug}/projects`; ReadSignals `{rooms_target unused, projects_title, projects_desc}` → service.CreateProject → SSE redirect to `/c/{slug}/projects/{id}`
3. [ ] `handler.GetProject` — GET `/c/{slug}/projects/{id}` → load Project + initial empty todos/attachments/comments → render `ProjectPage(data)`
4. [ ] `web/templ/projects.templ` — `ProjectPage(data)` with five `<section id="proj-{header,todos,attachments,comments,activity}">` placeholders; each renders an empty-state for now
5. [ ] CSS shell in `app.css` — minimal panel grid (two columns desktop, stacked mobile)

Verification: enter a title in the index "New project" form → land on `/c/main/projects/<id>` → see all five panel skeletons with the title rendered.

### Phase 3 — Datastar realtime: Bus + SSE + title/desc inline edit — status: open

Goal: title and description edits propagate to other open tabs within ~1s with no refresh.

1. [ ] `internal/projects/bus.go` — `Bus` with `SubscribeProject(projectID) (<-chan Event, func())` + `PublishProject(projectID, Event)`; copy structure from `internal/rooms/bus.go`
2. [ ] `handler.GetStream` — GET `/c/{slug}/projects/{id}/stream` returns SSE; subscribes to Bus, on each `Event` re-renders the affected fragment via `sse.PatchElementTempl(..., WithModeOuter())`
3. [ ] `service.UpdateTitle`, `service.UpdateDescription` + `handler.PostTitle`, `handler.PostDesc`; both publish `Event{Kind: "header"}`
4. [ ] Templ helpers `ProjectHeader(d)` and `ProjectDescription(d)` (called from `ProjectPage` and from SSE pushes)
5. [ ] Inline edit affordance in `projects.templ`: click title → editable input bound to `$projects_title` + datastar `data-on:keydown` on Enter → POST
6. [ ] Same affordance for description (textarea, markdown preview rendered server-side)
7. [ ] `web/static/projects.js` — bootstrap room-id from `data-init`; no per-element JS yet

Verification: two browser tabs as different members on the same project — tab A edits title → tab B's title updates within ~1s without refresh.

### Phase 4 — Todos panel (realtime) — status: open

Goal: collaborative todo list works across tabs in real time.

1. [ ] `repo` — `ListTodos(projectID)`, `InsertTodo`, `UpdateTodoBody`, `ToggleTodoDone`, `DeleteTodo`, `ReorderTodos`
2. [ ] `service` — wrap each with publish `Event{Kind: "todos"}`
3. [ ] `handler` — `POST /todo` (add), `POST /todo/{tid}` (edit), `POST /todo/{tid}/toggle`, `POST /todo/{tid}/delete`, `POST /todo/{tid}/sort`
4. [ ] Templ `ProjectTodos(d)` — checkbox + inline-editable body + delete; drag handle uses native HTML5 drag
5. [ ] `projects.js` — drag-end handler posts `/sort` with `{after_id}`; renumber done server-side
6. [ ] SSE stream extension: on `Event{Kind: "todos"}` re-render `#proj-todos`

Verification: tab A adds a todo → tab B sees it. Tab A toggles a checkbox → tab B sees the strike-through. Drag-reorder syncs across tabs.

### Phase 5 — Attachments — upload + download + delete — status: open

Goal: drag any file onto the attachments zone → all tabs see the row appear → click ⬇ → file downloads with original filename.

1. [ ] `internal/uploads` — add `Store.StreamAttachment(w, r, id) error` that sets `Content-Disposition: attachment; filename="<original>"` (or extend existing handler with a `?download=1` toggle, depending on shape — implementation choice)
2. [ ] `repo` — `InsertAttachment`, `ListAttachments`, `DeleteAttachment`
3. [ ] `service.AddAttachment(ctx, projectID, uploaderID, multipart.File, header)` — copies into uploads, inserts row, publishes `Event{Kind: "attachments"}`
4. [ ] `handler` — `POST /attachment` (multipart, possibly multi-file), `GET /attachment/{aid}/download`, `POST /attachment/{aid}/delete`
5. [ ] Permission check inside `DeleteAttachment`: uploader OR project creator OR community admin
6. [ ] Templ `ProjectAttachments(d)` — drop zone + file rows
7. [ ] `projects.js` — `dragover`/`dragleave`/`drop` on the zone; iterate `event.dataTransfer.files`; POST each as multipart
8. [ ] SSE extension on `Event{Kind: "attachments"}`

Verification: drag a PDF onto the zone → row appears in both tabs → click ⬇ → browser saves the file with the original name. Non-owner tab sees no delete button on someone else's file.

### Phase 6 — Comments — forum-style — status: open

Goal: project page has a SSE-driven comment thread with edit-grace + delete.

1. [ ] `repo` — `ListComments`, `InsertComment`, `UpdateComment`, `DeleteComment`
2. [ ] `service` — markdown render via `internal/render`; enforce edit-grace from `cfg.EditGrace`
3. [ ] `handler` — `POST /comment`, `POST /comment/{cid}`, `POST /comment/{cid}/delete`
4. [ ] Templ `ProjectComments(d)` — message list + textarea pinned to bottom
5. [ ] SSE extension on `Event{Kind: "comments"}`
6. [ ] Scroll-to-bottom on new comment (datastar `data-on-load` + small JS)

Verification: tab A posts a comment → tab B sees it appended. Edit within grace window works; outside it, the button is hidden.

### Phase 7 — Archive, hard delete, activity, permission polish — status: open

Goal: lifecycle complete + audit panel populated.

1. [ ] `repo` + `service` — `Archive`, `Unarchive`, `DeleteProject` (creator + admin only)
2. [ ] Index page renders archived projects under an expandable `<details>` block
3. [ ] `repo` — `RecentActivity(projectID, limit)` reads from a `project_activity` table OR derives from `_at` columns of other tables (decide at impl time — log table is simpler)
4. [ ] Templ `ProjectActivity(d)` — collapsed sidebar
5. [ ] Permission checks centralised — small `canEditProject`, `canDeleteAttachment`, etc helpers in `service.go`
6. [ ] CSS polish + mobile responsive review

Verification: archive a project → moves under "Archived" on index. Hard-delete (creator) — gone from DB. Activity panel shows recent events.

### Phase 8 — Spec sync + docs — status: open

Goal: spec mirrors the shipped reality; CHANGELOG entries land.

1. [ ] `/eidos:refine` pass on the spec — record any divergence (e.g., per-project file cap added during impl, or activity table shape decided)
2. [ ] CHANGELOG.md entries per phase
3. [ ] Decisions captured in `/eidos:decision` if any major Y/N was resolved during implementation

## Verification

End-to-end story we want green after Phase 7:

- Two browser windows, different users, same community, same project page open.
- User A: creates a project, edits the description, adds three todo items, drags a PDF and an .xlsx onto the attachments zone, posts a comment.
- User B (without refreshing): sees the title appear, description render with markdown, three checklist items populated, both files listed, the comment threaded below. Clicking ⬇ downloads the file with the original name.
- User B toggles a checkbox; user A sees the strike-through.
- User A archives the project; user B's index reflects the move.
- Restart the container; everything persists.
- Flip `PROJECTS_ENABLED=false` in `.env` and restart; nav link is gone, `/c/main/projects` returns 404, no data loss.

## Adjustments

<!-- Plans evolve. Document changes with timestamps. -->

## Progress Log

<!-- Updated after every completed action. -->
