---
name: spec-todos-personal-from-chat-and-forum
status: draft
type: spec
---

# Todos — personal lists derived from chat messages and forum posts

## Claim

Each user, inside a community that has the todos feature enabled, owns a private todo list. Entries are created by clicking "Add to todos" on any chat bubble or forum post; the source text is snapshotted at add-time AND a live backlink is preserved. The list lives at `/c/{slug}/todos`. Admin can toggle the feature per community.

## Behaviours

### Adding a todo

- A chat bubble shows an "Add to todos" button alongside the existing reply/bookmark/→thread row, **only when the community has todos enabled**.
- A forum post shows the same button under its `post-actions` row.
- Clicking opens the same kind of page-level dialog as bookmarks (single modal, `$todo_open_source` signal carries the source ID): title prefilled from `firstLine(BodyMarkdown)` (with image-only collapse to `(image)`, reusing `bookmarks.AutoTitleFromMarkdown`), optional category, optional note. Save POSTs `/c/{slug}/todos` with `source_kind` (`chat` | `forum_post`) and `source_id`.

### Source binding (snapshot + link)

- At creation, the server stores:
  - `body_snapshot` — the message body markdown at that moment (so edits/deletes don't change the todo).
  - `source_kind`, `source_id` — for the backlink.
  - For chat sources, also `source_day` (`YYYY-MM-DD` of the message in server-local TZ) so the backlink can target `/c/{slug}/history?d=...#msg-...` without re-querying.
  - For forum posts, also `source_thread_id` so the backlink builds `/c/{slug}/forum/{thread_id}#post-{post_id}`.

### Backlinks

- Chat-sourced row: `[open in history]` → `/c/{slug}/history?d={source_day}#msg-{source_id}`. The history page scroll-target is the source message; reading the ±30 messages of context is just reading the page.
- Forum-sourced row: `[open in thread]` → `/c/{slug}/forum/{source_thread_id}#post-{source_id}`.
- If the source has been deleted, the link still resolves to the parent (history day or thread) — the backlink degrades to "context only".

### Statuses

Three states stored as `status TEXT NOT NULL CHECK (status IN ('open','doing','done'))`:

- `open` — default at create
- `doing` — explicit in-progress (separate column instead of overloading category)
- `done` — completed

The list page header has tabs `[Active] [Doing] [Done] [All]` driven by `?status=` query param. `Active` is the default; it shows `open` + `doing`. `All` shows everything.

### Categories

Free-form string per todo (`category TEXT NOT NULL DEFAULT ''`). The filter dropdown enumerates distinct non-empty categories from the viewer's own rows (reuse the `bookmarks.DistinctCategories` shape). No admin-curated list.

### Page layout

`/c/{slug}/todos` page:

- Status tabs + category filter dropdown (sticky GET form).
- List ordered by `status ASC` (open → doing → done) then `created_at DESC`.
- Each row: checkbox (toggles open ↔ done), status pill (Open / Doing / Done), title (editable inline like rename), category chip, backlink button, delete button.
- "Mark doing" click sets `doing`; click again sets `open`. Done click sets `done`; click again sets `open`.

### Per-community feature toggle

- New column `communities.todos_enabled INTEGER NOT NULL DEFAULT 0`.
- Admin (per-community) page gains a "Todos feature: [enable | disable]" control.
- When disabled:
  - The `Todos` nav link is hidden in the topbar.
  - `Add to todos` buttons are hidden on chat / forum.
  - `/c/{slug}/todos` returns 404.
  - Existing rows in the DB are preserved; re-enabling restores access.

## Schema

```sql
ALTER TABLE communities ADD COLUMN todos_enabled INTEGER NOT NULL DEFAULT 0;

CREATE TABLE todos (
    id              TEXT PRIMARY KEY,
    community_id    TEXT NOT NULL REFERENCES communities(id) ON DELETE CASCADE,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_kind     TEXT NOT NULL CHECK (source_kind IN ('chat','forum_post')),
    source_id       TEXT NOT NULL,
    source_thread_id TEXT,                              -- forum_post only
    source_day      TEXT,                               -- chat only, YYYY-MM-DD
    title           TEXT NOT NULL,
    body_snapshot   TEXT NOT NULL DEFAULT '',
    category        TEXT NOT NULL DEFAULT '',
    note            TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','doing','done')),
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    completed_at    INTEGER
);
CREATE INDEX idx_todos_user_status_created ON todos(user_id, community_id, status, created_at);
CREATE INDEX idx_todos_user_category ON todos(user_id, community_id, category);
```

Idempotence is not enforced at the DB level: a user can add the same source twice on purpose (re-add after deletion, or duplicate to split). The UI may dim the button when an open todo for that source already exists, but not enforce.

## Interactions

Affects:

- `internal/chat/handler.go`, `internal/forum/handler.go` — new `data-on:click` buttons in the templ (toggled by `v.TodosEnabled`).
- `web/templ/chat.templ`, `web/templ/forum.templ` — render the button + carry todos-enabled flag.
- `web/templ/layout.templ` — `Viewer` gains `TodosEnabled bool`; topbar shows the Todos link conditionally.
- `internal/community/community.go` — add `Todos enabled` field on `Community`; migration.
- `internal/admin/admin.go` — feature-toggle endpoint.
- `internal/history/history.go` — emit `id="msg-{id}"` on each chat row so the fragment scroll target lands.
- `internal/forum/handler.go` — already emits `id="post-{id}"`, no change.
- `internal/storage/sqlite/migrations/00006_todos.sql` — new migration.
- New `internal/todos/` package — repo + handler.
- `web/templ/todos.templ` — new page.

Depends on:

- `bookmarks.AutoTitleFromMarkdown` for image-only collapse — duplicated regex pattern or refactored into a tiny shared helper (`internal/render/snippet.go` candidate).
- Per-community routing (`/c/{slug}` group) for all endpoints.
- `community.FromContext` + `auth.FromContext` for scoping.

## Verification

- Toggle feature off → topbar drops the Todos link, "Add to todos" buttons disappear, direct GET `/c/{slug}/todos` returns 404.
- Add a chat message to todos → row appears with chat-source backlink → click → history page scrolls to message highlighted.
- Edit then delete the source chat message → backlink still resolves to the history day, body_snapshot in the todo unchanged.
- Add same message twice → two rows. Both can be marked done independently.
- Change status with click — round-trips through `open → doing → done → open`.
- Category filter only shows the user's own distinct values.
- A non-member of the community gets 404 on `/c/{slug}/todos` regardless of feature flag.

## Friction / Trade-offs

- `body_snapshot` duplicates content. For typical workloads (a handful of todos per user) cost is trivial; if growth becomes an issue, switch to live join on `chat_messages` / `posts` with deletion-aware rendering.
- The chat backlink relies on `source_day` rather than re-deriving from the message timestamp at click time. This means timezone changes (user moves servers across TZ) could make older backlinks land on the wrong day. Acceptable: history is server-local and stable.
- "Add to todos" sits alongside "bookmark" and the boundary is fuzzy. Decision: bookmarks are passive saves; todos are actionable. UI copy should reflect this ("Bookmark for later" vs "Track as todo").

## Future

- Due-dates / reminders — explicitly out of scope for v1.
- Shared community todos — would be a second surface (`/c/{slug}/team-todos`) with the same schema plus a `shared bool`. Easy to add later.
- Editable backlinks (re-target a todo to a different source) — out of scope.
- Markdown rendering of body_snapshot in the todo list — show plain text excerpt; render only on detail/modal.

## Status

draft — ready for `/eidos:plan` to break down implementation phases (schema → repo → handler → templ → admin toggle).
