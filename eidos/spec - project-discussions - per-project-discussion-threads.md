---
name: spec-project-discussions-per-project-threads
status: draft
type: spec
tldr: Forum-style discussion threads scoped to one project. Subject + body + replies with quoted-reply support, image attachments, edit-grace + soft-delete. Members + share-link guests both read and write. Renders as a new "Discussions" tab between Issues and Comments.
---

# Project Discussions — per-project forum threads

## Target

Issues capture tickets with a status workflow. Project-level comments are a single linear thread. Neither is the right shape for an open-ended question or design conversation. Discussions fill the gap: a threaded forum surface scoped to one project, mirroring `internal/forum` but per-project instead of per-community.

## Behaviour

- Tab strip gains a "Discussions" entry between Issues and Comments.
- `/c/{slug}/projects/{id}/discussions` — thread list, newest first by `last_activity_at`.
- `/c/{slug}/projects/{id}/discussions/new` — implicit (form on the index page).
- `/c/{slug}/projects/{id}/discussions/{did}` — single thread + replies.

### Thread model

- Subject (≤200 chars), body markdown.
- Author identity captured via `creator_user_id` xor `creator_guest_id` + `creator_name` snapshot — same pattern as issues.
- `last_activity_at` bumps on every reply so active threads float to the top.
- Soft-delete (`deleted_at`); admin or author can delete.

### Replies

- Body markdown, optional `quoted_reply_id` pointing at another reply in the same thread (forum's flat-quote pattern).
- Edit-grace window matches `cfg.EditGrace` (15min). Author + admin can edit within grace; admin always.
- Soft-delete by author + admin.

### Permissions

| Action                              | Member | Guest |
|-------------------------------------|:------:|:-----:|
| Read thread list / thread view      | ✓      | ✓     |
| Create new thread                   | ✓      | ✓     |
| Reply on any thread                 | ✓      | ✓     |
| Edit own thread / reply (within grace) | ✓   | ✓     |
| Delete own thread / reply           | ✓      | ✓     |
| Delete anyone's                     | admin  | ✗     |

### Image attachments

- Drag-drop or paste an image into the thread body or reply body composer.
- Reuse `uploads.Store.Save` (image whitelist). Guests attribute to the project creator as owner — same fix as issue attachments (commit `cd149de`).
- Rendered inline in `body_html` via the existing markdown image pipeline.

### Realtime

- Phase 1+2: page reload on POST (same shortcut used on issues — commit `37caa98`). Per-thread SSE stream is future work alongside the same upgrade for the issue page.

## Schema

```sql
CREATE TABLE project_discussion_threads (
    id               TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    subject          TEXT NOT NULL,
    body_md          TEXT NOT NULL DEFAULT '',
    body_html        TEXT NOT NULL DEFAULT '',
    creator_user_id  TEXT,
    creator_guest_id TEXT,
    creator_name     TEXT NOT NULL,
    deleted_at       INTEGER,
    last_activity_at INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
);
CREATE INDEX idx_project_discussions_project_activity
    ON project_discussion_threads(project_id, last_activity_at DESC);

CREATE TABLE project_discussion_replies (
    id              TEXT PRIMARY KEY,
    thread_id       TEXT NOT NULL REFERENCES project_discussion_threads(id) ON DELETE CASCADE,
    quoted_reply_id TEXT REFERENCES project_discussion_replies(id) ON DELETE SET NULL,
    author_user_id  TEXT,
    author_guest_id TEXT,
    author_name     TEXT NOT NULL,
    body_md         TEXT NOT NULL,
    body_html       TEXT NOT NULL,
    edited_at       INTEGER,
    deleted_at      INTEGER,
    created_at      INTEGER NOT NULL
);
CREATE INDEX idx_project_discussion_replies_thread
    ON project_discussion_replies(thread_id, created_at);
```

Image attachments piggyback on the existing `uploads` table — no extra link table needed, because images live inline in the markdown body via the existing `/uploads/{id}?sig=...` pattern.

## Design

### Package layout addition

```
internal/projects/
  discussions.go            — Thread / Reply types
  discussions_repo.go       — SQL
  discussions_service.go    — CRUD + permission checks
  discussions_handler.go    — HTTP + tab GET / POST endpoints
web/templ/
  projects.templ            — ProjectDiscussionsPage + ProjectDiscussionThreadPage + view structs
```

### Routes

Inside `r.Route("/c/{slug}/projects")` → open group (guest OR member, no auth-required middleware):

```
GET    /{id}/discussions                              list
POST   /{id}/discussions                              create thread
GET    /{id}/discussions/{did}                        thread page
POST   /{id}/discussions/{did}                        edit thread (author + admin)
POST   /{id}/discussions/{did}/delete                 delete thread
POST   /{id}/discussions/{did}/reply                  reply
POST   /{id}/discussions/{did}/reply/{rid}            edit reply
POST   /{id}/discussions/{did}/reply/{rid}/delete     delete reply
```

`{did}` and `{rid}` are UUIDs — no chi URL-decode trap.

## Verification

- Member on the project page → Discussions tab → "New thread" form → submit → land on thread page.
- Guest at the share URL → Discussions tab visible → opens new thread "Where should the API key live?" → admin sees it.
- Admin replies "Use env var." → guest sees the reply on refresh.
- Guest quotes the admin's reply → quoted block renders above the new reply body.
- Author edits within 15min → "(edited)" stamp appears; outside grace the Edit button is hidden.
- Author deletes own reply → tombstone collapses the row.
- Discussion-tab nav highlight applies on `/discussions` and `/discussions/{did}`.

## Friction

- Realtime stays page-reload for v1 — same trade-off as issues. Per-thread SSE stream is in Future.
- Quoted-reply renders inline in the body via templ's quote block (matches `internal/forum`'s flat-quote shape).
- Image attachments use the same image-only whitelist as issues; documents on discussions are out of scope for v1.
- Activity panel does NOT include discussion events. Logged as Future alongside the same gap for issues.

## Interactions

- Depends on [[spec - projects - per-community-collaborative-projects]] (project shell, tabs, layout).
- Depends on [[spec - project-issues - per-project-issues-with-guest-share-links]] (callerIdentity helper, guest session pattern, route split).
- Depends on `internal/forum` (only as a reference for the thread+post+quote shape; not as a runtime dep).
- Reuses `internal/render.RenderMarkdown` + the image-upload signed-URL pipeline + `uploads.Store.Save`.

## Mapping

> [[internal/projects/discussions.go]]
> [[internal/projects/discussions_repo.go]]
> [[internal/projects/discussions_service.go]]
> [[internal/projects/discussions_handler.go]]
> [[web/templ/projects.templ]] (new ProjectDiscussions* templs)
> [[internal/storage/sqlite/migrations/0001N_project_discussions.sql]]
> [[cmd/app/main.go]] (route mounts)

## Future

- {[!] per-thread SSE stream — replaces the reload with seamless morph (joint upgrade with issue page)}
- {[?] mark thread as resolved / pinned}
- {[?] subscribe to thread (notification on new reply)}
- {[?] move thread to a different project}
- {[?] full-text search across all discussions in a community}

## Notes

- Following the same datastar virtual-DOM pattern: state struct → templ fragment → SSE patch. Phase 4 of project-issues established the reload shortcut; discussions inherit it.
- Tab label is "Discussions" (per the agreed labels Overview / Todos / Docs / Issues / Discussions / Comments / Activity). Adding a 7th tab is fine — strip already scrolls horizontally on mobile.
