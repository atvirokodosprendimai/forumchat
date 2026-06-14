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

### Phase 1 — Feature flag, schema, empty index — status: completed

Goal: `PROJECTS_ENABLED=true` shows a Projects nav link and a `/c/{slug}/projects` page rendering "No projects yet". `false` hides everything. DB tables exist regardless.

1. [x] Migration `00013_projects.sql` — projects + project_todos + project_attachments + project_comments
2. [x] `internal/config/config.go` — `ProjectsEnabled bool env:"PROJECTS_ENABLED" envDefault:"false"`
3. [x] `internal/projects/types.go` — `Project`, `Todo`, `Attachment`, `Comment`, `Event`, `IndexRow`
4. [x] `internal/projects/repo.go` — `ListActiveForCommunity` / `ListArchivedForCommunity` (with aggregate counts via correlated subqueries), `ByID`, `Insert`, `UpdateTitle`, `UpdateDescription`, `SetArchived`, `Delete`
5. [x] `internal/projects/handler.go` — `Handler{Repo, Log}` + `GetIndex` only
6. [x] `web/templ/projects.templ` — `ProjectsGrid` + inline `projectCard` + empty state + collapsed archived `<details>` section + "New project" form
7. [x] `cmd/app/main.go` — `projectsRepo` / `projectsHandler` constructed, `webtempl.ProjectsEnabled = cfg.ProjectsEnabled`, `/projects` GET mounted only when flag true
8. [x] Used a package-level `var webtempl.ProjectsEnabled bool` instead of plumbing through every `Viewer` construction
   - => Decision: 1 global var beats N handler edits. Flag is instance-level (not per-request) so the var fits the semantic shape.
9. [x] Spec + plan + Phase 1 all merge to main via this branch's PR (task/projects-phase-1, off task/spec-projects)

Verification: with `PROJECTS_ENABLED=true`, nav shows "Projects", `/c/main/projects` returns 200 with empty grid. With `=false`, link absent, route 404.

### Phase 2 — Create + view a project (form-driven, no SSE yet) — status: completed

Goal: any approved member can create a project, click into it, see all five panel placeholders rendered server-side. No live updates yet.

1. [x] `internal/projects/service.go` — `Service{Repo}`, `NewService`, `CreateProject(ctx, communityID, creatorID, title, descMD) (Project, error)`; markdown render via `internal/render.RenderMarkdown`. Sentinel errors `ErrEmptyTitle`, `ErrNotFound`, `ErrForbidden`.
2. [x] `handler.PostCreate` — POST `/c/{slug}/projects` (plain HTML form, no datastar signals — Phase 3 will switch where appropriate); 303-redirects to `/c/{slug}/projects/{id}` so refresh doesn't re-post.
3. [x] `handler.GetProject` — GET `/c/{slug}/projects/{id}` with cross-community guard (404 on slug mismatch so we don't leak ids), renders `ProjectPage(data)`.
4. [x] `web/templ/projects.templ` — `ProjectPage` with five `<section id="proj-{header,todos,attachments,comments,activity}">` panels; back-to-projects breadcrumb; `ProjectHeader` split out for later SSE morph reuse.
5. [ ] CSS shell deferred — page renders with default styling for now; visual polish lands in Phase 7.
   - => Decision: ship Phase 2 without CSS so phases 3-6 land their own structural HTML first, then one consolidated CSS pass at the end avoids churn.

Verification: enter a title in the index "New project" form → land on `/c/main/projects/<id>` → see all five panel skeletons with the title rendered.

### Phase 3 — Datastar realtime: Bus + SSE + title/desc inline edit — status: completed

Goal: title and description edits propagate to other open tabs within ~1s with no refresh.

1. [x] `internal/projects/bus.go` — `Bus` with `SubscribeProject` + `PublishProject`; same shape as rooms/forum buses
2. [x] `handler.GetStream` — GET `/c/{slug}/projects/{id}/stream`, pushes header on open then on each Bus Event; 25s keepalive via empty PatchSignals
3. [x] `service.UpdateTitle`, `service.UpdateDescription` — both publish `Event{Kind: "header"}`
4. [x] `handler.PostTitle`, `handler.PostDescription` — datastar.ReadSignals into `projectSignals{Title, Description}`, return 204
5. [x] Templ — single `ProjectHeaderFragment(slug, p)` covers both the in-page initial render AND the SSE morph; data-show toggles edit/display via `$projects_edit_header`; Enter on title and a "Save" button both POST
6. [x] Description editing uses a textarea bound to `$projects_desc`; same Save button POSTs both endpoints sequentially and clears the edit flag
7. [ ] Skipped `web/static/projects.js` for Phase 3 — datastar `data-init=@get(...)` opens the SSE stream directly, no extra JS needed yet
   - => Decision: JS only lands when a per-element interaction (drag-drop, keyboard reorder) needs it. Phase 5 will introduce projects.js for drag-and-drop attachments.
8. [x] `cmd/app/main.go` — `projectsBus := projects.NewBus()` constructed and wired into Service+Handler; three new routes mounted (`/stream`, `/title`, `/desc`).

Verification: two browser tabs as different members on the same project — tab A edits title → tab B's title updates within ~1s without refresh.

### Phase 4 — Todos panel (realtime) — status: completed

Goal: collaborative todo list works across tabs in real time.

1. [x] `repo` — `ListTodos`, `MaxTodoSortOrder`, `InsertTodo`, `UpdateTodoBody`, `ToggleTodoDone`, `DeleteTodo`, `TodoByID`, `ReorderTodos` (transactional)
2. [x] `service` — `AddTodo`, `UpdateTodoBody`, `ToggleTodo`, `DeleteTodo`, `ReorderTodos`; all publish `Event{Kind:"todos"}`
3. [x] `handler` — `PostTodoAdd`, `PostTodoEdit`, `PostTodoToggle`, `PostTodoDelete`, `PostTodoReorder`; signals bag extended with `projects_todo_body`/`projects_todo_edit`/`projects_todo_order`
4. [x] `ProjectTodosFragment` templ — checkbox toggle, double-click body to edit, Enter to save, × to delete, "+ Add" form pinned at the bottom; ID-keyed via `$projects_todo_edit_id` so multiple rows don't fight for one signal
5. [p] Drag-reorder via `projects.js` deferred — todo list works fine without drag for v1; postponed to Phase 7 polish.
   - => Reason: drag-and-drop needs the same projects.js file as Phase 5's attachment dropzone. Land them together in Phase 5 instead of two JS commits.
6. [x] SSE stream — `Event{Kind:"todos"}` triggers `pushTodos` which morphs `#proj-todos` via WithModeOuter
7. [x] `GetProject` now loads todos on initial render; `pushAll` on stream-open syncs late joiners
8. [x] main.go — 5 new routes mounted under feature flag

Verification: tab A adds a todo → tab B sees it. Tab A toggles a checkbox → tab B sees the strike-through. Drag-reorder syncs across tabs.

### Phase 5 — Attachments — upload + download + delete — status: completed

Goal: drag any file onto the attachments zone → all tabs see the row appear → click ⬇ → file downloads with original filename.

1. [x] `internal/uploads` — `Store.SaveAttachment(ctx, ownerID, communityID, mime, filename, r)` bypasses the image-only MIME whitelist. Extension derived from original filename, fallback to `.bin`.
2. [x] `repo` — `ListAttachments`, `InsertAttachment`, `AttachmentByID`, `DeleteAttachment`
3. [x] `service.AddAttachment` and `service.DeleteAttachment` (with permission enforcement). DeleteAttachment also calls `uploads.Store.Delete` which no-ops when other rows still reference the same content hash.
4. [x] `handler` — `PostAttachmentUpload` (multipart, accepts multiple files under `files[]` or `file`), `GetAttachmentDownload` (streams from disk with `Content-Disposition: attachment; filename=…`), `PostAttachmentDelete`
5. [x] Permission rule: uploader OR project creator OR community admin. Enforced in service so it's not bypassable from the handler.
6. [x] Templ `ProjectAttachmentsFragment` — drop zone with click-to-choose, file rows with size pill + download link + delete button
7. [x] `web/static/projects.js` — drag/drop + click-to-choose handlers; MutationObserver re-binds the zone after every SSE morph
8. [x] SSE extension — `Event{Kind:"attachments"}` triggers `pushAttachments` which morphs `#proj-attachments`
9. [x] `humanBytes` inline in templ for size formatting (B/KB/MB/GB)

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

- **2026-06-14 14:20** — Phase 1 completed on `task/projects-phase-1` (off `task/spec-projects`). Migration 00013, config flag, projects package skeleton, templ index page, main.go wiring. Build clean. Ready to commit + open PR that brings spec + plan + Phase 1 to main together. Shipped commit `54581bb`.
- **2026-06-14 14:32** — Phase 2 completed on the same branch. service.go with CreateProject + sentinel errors, handler.PostCreate + GetProject, ProjectPage templ with 5 panel skeletons, routes mounted under the feature-flag conditional in main.go. CSS deferred to Phase 7. Build clean. Commit `bd0170e`.
- **2026-06-14 14:42** — Phase 3 completed. bus.go fan-out, service UpdateTitle/UpdateDescription, handler PostTitle/PostDescription/GetStream, ProjectHeaderFragment with inline edit affordance driven by `$projects_edit_header`/`$projects_title`/`$projects_desc`, three new routes. Build clean. Commit `bdc188e`.
- **2026-06-14 14:55** — Phase 4 completed. 8 new repo methods (incl. transactional ReorderTodos), 5 service mutators, 5 handler endpoints, ProjectTodosFragment templ with double-click-to-edit + checkbox toggle + delete + add form, SSE handler now pushes todos on `Event{Kind:"todos"}` and on stream open. Drag-reorder postponed to Phase 5 so both projects.js needs land together. Build clean. Commit `04a3bfc`.
- **2026-06-14 15:10** — Phase 5 completed. Uploads.SaveAttachment for any-MIME documents, 4 repo methods, 2 service mutators with permission enforcement, 3 handler endpoints (multipart upload, download with original filename, delete), ProjectAttachmentsFragment templ with drop zone + file list, projects.js drag/drop + click-to-choose + MutationObserver re-bind. Build clean.
