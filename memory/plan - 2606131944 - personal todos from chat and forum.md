---
tldr: Implement personal-per-user todos in a community, derived from chat messages and forum posts, with snapshot text + live backlinks. Per-community feature toggle. Three statuses (open/doing/done), free-form category, dedicated page at /c/{slug}/todos.
status: active
---

# Plan: Personal todos from chat and forum

## Context

- Spec: [[spec - todos - personal-todos-from-chat-and-forum]] (commit `aba4cea`)
- Prior multi-community refactor: routes nest under `/c/{slug}/...`, viewer carries `CommunitySlug`, handlers read community from context via `cid()`/`cname()`/`cslug()`.
- Existing patterns to reuse:
  - `bookmarks.AutoTitleFromMarkdown` and `SnippetForList` — image-only collapse to `(image)`.
  - Bookmark dialog pattern (`BookmarkDialog(slug)` — single page-level modal driven by a signal).
  - Free-form category UI pattern from bookmarks (`DistinctCategories` + dropdown).
- Plan-shape decisions:
  - Phase order optimises for **visible result early** — a usable todo page (even if empty) lands first.
  - Add buttons land **before** backlink polish, so user feels the loop closing.
  - Per-community toggle and admin UI come last so the feature can be tested by always-enabled boot community while plumbing settles.

## Phases

### Phase 1 — Schema + repo + minimal page — status: completed

Goal: `/c/{slug}/todos` renders an empty list (no add buttons yet). DB ready.

1. [x] Migration 00006 — communities.todos_enabled flag + todos table + indexes
   - => `internal/storage/sqlite/migrations/00006_todos.sql`. Toggle defaults to 0 (off).
2. [x] `internal/todos/todos.go` — `Todo` struct + `Repo` with `Create`, `ListForUser`, `ByID`, `UpdateStatus`, `UpdateTitle`, `Delete`, `DistinctCategories`
   - => `scanTodo` helper; status enum constants `StatusOpen`/`StatusDoing`/`StatusDone`.
   - => ListForUser status semantics: `""` and `"active"` both → `open`+`doing`; `"all"` → no filter.
3. [x] `internal/todos/handler.go` — `GetIndex`
   - => `MustFromContext` for community (page is mounted under `/c/{slug}` group).
4. [x] `web/templ/todos.templ` — `TodosPage`, `TodosList`, `TodoRowView` partials
   - => Filter pills reuse `.forum-filters` CSS class.
   - => Backlink href builder `todoBacklinkHref` already does the chat-history + forum-thread shapes ready for Phase 4 — just needs `id="msg-..."` on history rows.
5. [x] Wire route `r.Get("/todos", todosHandler.GetIndex)` in `cmd/app/main.go` community group.
6. [x] Add `Todos` link to topbar nav in `layout.templ` (always visible for now).
7. [x] CSS for `.todo-row`, `.todo-pill`, status pill colors in `app.css`.

Verification: hard-reload `/c/{slug}/todos`, page renders with filter UI and "no todos yet" message. Build clean.

### Phase 2 — Add to todos from chat — status: completed
   - => Done in same commit as Phase 3 (forum) since the dialog is shared.

Goal: clicking "Add to todos" on a chat bubble creates a row that appears on the page.

1. [ ] Add `TodoDialog(slug string)` to `web/templ/todos.templ` — single page-level modal mirroring `BookmarkDialog`
   - Driven by signals `$todo_open_source` (composite `"chat:<msgID>"` or `"forum_post:<postID>"`), `$todo_title`, `$todo_category`, `$todo_note`.
   - Layout signal additions in `layout.templ`.
2. [ ] Render `@TodoDialog(d.Viewer.CommunitySlug)` once in `ChatPage`
3. [ ] Add "Add to todos" button to `MessageView` in `chat.templ` (next to bookmark)
   - `data-on:click` sets `$todo_open_source = 'chat:' + m.ID` and prefills `$todo_title` from `m.AuthorName` + first-line snippet (compute server-side in MsgView).
   - `MsgView` gains `TitleSnippet string` populated in `chat.toMsgView` (call `bookmarks.AutoTitleFromMarkdown(m.BodyMarkdown)` — extract helper to `internal/render/snippet.go` if it grows uncomfortable).
4. [ ] `todos.PostCreate` handler
   - ReadSignals → parse `$todo_open_source` into `source_kind` + `source_id`.
   - For chat: look up message via `chat.Repo.ByID`, snapshot body_md, derive `source_day` from `created_at` in server-local TZ.
   - INSERT row, redirect (or PatchSignals to clear + outer-morph the list if user is on the page).
   - Same handler will serve forum sources in Phase 3.
5. [ ] Route `POST /c/{slug}/todos` in main.go community group.

Verification: open chat, click "Add to todos" on a bubble, fill title, save, navigate to `/c/{slug}/todos`, row appears with chat-source label.

### Phase 3 — Add to todos from forum posts — status: completed
   - => internal/render.AutoTitle extracted (shared with bookmarks helper).
   - => MsgView.TitleSnippet + PostView.TitleSnippet populated by handlers.
   - => TodoDialog(slug) rendered once on ChatPage and ThreadPage.
   - => todos.PostCreate handles chat: + forum_post: sources; cleans dialog signals on success.

Goal: same loop from forum thread posts.

1. [ ] Add "Add to todos" button to `ForumPost(slug, threadID, p)` in `forum.templ`
   - `data-on:click` sets `$todo_open_source = 'forum_post:' + p.ID` and prefills title.
   - `PostView` already carries `BodyHTML`; add `TitleSnippet` field analogous to `MsgView` change.
2. [ ] Extend `todos.PostCreate` to handle `forum_post:` source
   - Look up via `forum.Repo.GetPost(postID)` → snapshot body_md, store `source_thread_id`.
3. [ ] Render `@TodoDialog(slug)` once in `ThreadPage`

Verification: in a forum thread, click "Add to todos" on a reply, save, todo row appears with thread-source label.

### Phase 4 — Backlinks land at the right place — status: open

Goal: clicking the backlink button on a todo opens the source in context.

1. [ ] Chat backlink → `/c/{slug}/history?d=<source_day>#msg-<source_id>`
   - `history.templ` row HTML currently has no `id` attribute per chat row — add `id={ "msg-" + e.ID }` to chat events. (History also needs the source ID stored; check `HistoryEvent`.)
   - If `HistoryEvent` doesn't currently expose source ID, add it and plumb through `eventsBetween`.
2. [ ] Forum backlink → `/c/{slug}/forum/<source_thread_id>#post-<source_id>`
   - `forum.templ` already emits `id={ "post-" + p.ID }` — no change.
3. [ ] Todo row renders the backlink button conditionally:
   - chat source → "open in history" link.
   - forum_post source → "open in thread" link.

Verification: scroll-into-view lands on the highlighted source message in both surfaces.

### Phase 5 — Status transitions + delete + filter — status: open

Goal: full lifecycle.

1. [ ] `todos.PostStatus` — `POST /c/{slug}/todos/{id}/status` with `next` field cycling open→doing→done→open.
2. [ ] Per-row buttons: checkbox toggles open↔done, "doing" pill toggles open↔doing.
3. [ ] `todos.PostDelete` — `POST /c/{slug}/todos/{id}/delete`.
4. [ ] Filter UI:
   - Status tabs hit `GET /c/{slug}/todos?status=active|doing|done|all`.
   - Category dropdown changes `?category=...`.
   - Both preserve the other via hidden inputs (mirror forum search pattern).

Verification: click status cycles work, delete removes the row, filters compose.

### Phase 6 — Per-community feature toggle — status: open

Goal: admin can switch the feature off; UI vanishes correctly.

1. [ ] Migration check — `communities.todos_enabled` already in 00006; no schema change here.
2. [ ] `community.Repo.SetTodosEnabled(communityID, on bool)`.
3. [ ] Admin page section: "Todos feature" — toggle button + form.
4. [ ] `admin.PostSetTodosEnabled` — POST in the per-community admin group.
5. [ ] Viewer struct gains `TodosEnabled bool`; populated in each handler's `viewer(r)` via `community.MustFromContext(r.Context()).TodosEnabled`.
6. [ ] Conditional rendering:
   - Topbar Todos nav link hidden when `!v.TodosEnabled`.
   - "Add to todos" buttons hidden on chat + forum when `!v.TodosEnabled`.
   - `GET /c/{slug}/todos` returns 404 when flag off (middleware `todos.RequireEnabled`).

Verification: flip flag in admin, every surface drops the feature; flip back, everything returns. Existing rows preserved across toggle.

### Phase 7 — Polish + verification sweep — status: open

1. [ ] Verify spec acceptance list end-to-end (delete-source still resolves to parent, duplicate add allowed, non-member 404).
2. [ ] CSS pass — status pills, category chip, doing/done visual states.
3. [ ] Sanity check the bookmark/todo distinction in UI copy ("Bookmark for later" vs "Track as todo").
4. [ ] Update [[spec - todos - personal-todos-from-chat-and-forum]] status from `draft` → `implemented`.

## Verification (overall)

- Bookmark + todo do not interfere; both buttons coexist on chat bubble.
- Feature toggle round-trips cleanly across all surfaces.
- Backlink degradation: delete the source chat message → todo row still shows, backlink lands on history day (just without the highlighted message).
- Build clean, vet clean, no LSP-cache-only issues at boundary commits.

## Adjustments

(Empty — log timestamped reasons here when phases get reshaped.)

## Progress Log

- 2606131944 — plan created from spec.
- 2606131958 — Phase 1 completed. Migration 00006, todos package, GetIndex, TodosPage templ, route + nav. Build+vet clean.
- 2606132025 — Phases 2+3 completed in one commit. Add-to-todos buttons live on chat bubbles AND forum posts. TodoDialog modal. PostCreate handles both source kinds.
