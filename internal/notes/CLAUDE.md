# notes

Community shared notes ("iNotes"). A member writes a note in **markdown** on a
dedicated page; it renders to sanitized **HTML** for reading and carries inline
comments anchored to the rendered blocks. The pastebin sibling (`internal/pastes`)
was the clone template; notes diverge as a *listed collection*, *repeatedly
editable*, with a *visibility split* and *inline comments*.

## Model

- **`notes`** (migration 00063): `body` (markdown source, the editor) + `body_html`
  (rendered+sanitized, the reader) + `visibility` (`public`|`private`) +
  `share_token` (32-byte capability, minted at draft-create so the copy link is
  correct before first save).
- **`note_comments`** (00063): `block_index` (0-based top-level rendered block the
  comment anchors to) + `quote` (selected-text snippet; `''` = whole-line) +
  `resolved_at`. A comment whose `block_index >=` the live block count is
  **orphaned** (the note was edited under it) and shown in the margin, not moved.

## Authority (one place each — don't re-derive in handlers)

Two tiers, deliberately split (a granted collaborator edits CONTENT only; the
note's owners still control distribution + lifecycle):

- **`Note.CanManage(id)`** = author OR `Role.AtLeast(RoleMod)` OR `IsSuperAdmin`.
  The policy authority — the pre-grant meaning of "editor". Managers approve
  edit-rights requests, change **visibility**, **share**, **delete**, and
  moderate others' comments.
- **`Note.CanEdit(id)`** = `CanManage` OR a member granted edit rights via the
  request-to-edit flow (`Note.GrantedEditors`, populated by `Repo.ByID`).
  Gates content editing only: the editor zone, `Save` (body/title), `SyncBody`,
  `GetCollab`.
- **`Comment.CanModerate(id, note)`** = comment author OR `note.CanManage` (NOT
  CanEdit — a granted collaborator doesn't curate others' comments).
- **`Service.Save`** enforces `n.CommunityID == in.CommunityID` (no cross-tenant
  write — a mod of community B must not save a note from A) AND `CanEdit`; it
  applies `note_visibility` **only** when `CanManage` (a collaborator's forged
  visibility signal is ignored). The community guard is load-bearing; a Codex
  pass caught its absence.

### Request to edit (migration 00075 `note_edit_requests`)

A regular member can read + comment a PUBLIC note but, by design, never edit it
— which also blocked the whole community from the collaborative diff-sync
editor. The fix is a per-note grant ACL, opt-in one note at a time:

- A non-editor member sees a **"Request edit rights"** button (the
  `#note-access` panel, `NoteAccessPanel`); OTHER members only — an editor never
  sees it. `Service.RequestEdit` records a `pending` row (idempotent on
  `(note_id, user_id)` via `ON CONFLICT DO NOTHING`; an already-editor caller
  gets `ErrAlreadyEditor`). A request on a missing / cross-community / private
  note all collapse to `ErrForbidden` → flat 404 (no existence oracle).
- A **manager** sees the pending requests + current collaborators in the same
  panel and `Service.DecideEditRequest(grant)`s: `grant=true` flips the row to
  `granted` (→ `CanEdit` true → the member gets the live editor on next load);
  `grant=false` DELETEs (declines a pending row OR revokes a grant — no row =
  no relationship, re-requestable). **Manager-only** (`CanManage`, not CanEdit)
  so a granted collaborator can't escalate by granting others — Codex-checked.
- `Repo.ByID` loads `GrantedEditors` on every single-note read (tiny indexed
  lookup) so `CanEdit` is uniform across handler + service paths. List/token
  reads don't load grants (don't need them).
- Handlers call the service BEFORE `render.NewSSE` (errors reply with a plain
  status; only success/already-editor patch the panel) — opening SSE first then
  `http.Error` would emit a garbled second header (the project's NewSSE caveat).
- Approval is not pushed cross-session: the approved member gains the editor on
  their next page load (they hold no collab stream while non-editor).

## Visibility & the public token reader

- **public** → listed in `/c/{slug}/notes`, readable by any approved member at
  `/c/{slug}/notes/{id}`.
- **private** → NOT listed; `GetPage` 404s it for non-editors. The ONLY non-editor
  read path is the capability link **`GET /n/{token}`** (`GetShared`), mounted at
  router root, identity-optional, no approval gate — the token is the bearer
  capability (like the data-export links, AGENTS §5h). Any miss (bad token, gone,
  wrong) renders the same generic `NoteUnavailable` page — no existence oracle.
  The comments rail is hidden on the shared view (internal collaboration surface;
  don't leak commenter identities to an anon link-holder).

## Inline comments (line + selection)

- `render.AnnotateBlocks(html)` tags each top-level rendered block `data-nb="<i>"`
  and returns the count (the anchor map). Run at display time on already-sanitized
  HTML; the attribute is our own trusted output.
- `web/static/note.js` layers interaction: a text-selection "💬 Comment" button, a
  per-line gutter "+", per-block "💬 N" badges, and jump-to-block highlight — all
  emit ONE `fc:note-comment` custom event `{block, quote}` that a single Datastar
  listener on `#note-reader` consumes (EDA, AGENTS §4.12). **Detach the
  MutationObserver while painting** (`paint()`), else badge writes re-trigger it →
  infinite loop (cost: a hung tab; caught in verification).
- The whole reader (`#note-reader`) is one stable-id fat-morph (AGENTS §4.7): save
  and every comment add/resolve/delete re-render it in place.

## Share to channel

`PostShare` (editors only) posts the note URL into a channel via the `PostToChat`
closure wired in main.go (no chat import cycle, like pastes). Public → member
route; private → absolute `/n/{token}`. The link is built from the note's
**persisted** visibility/token (`shareLink`), so a stale editor signal can't post
the wrong URL. Note: chat flattens `[label](url)` to the bare URL
(`render.sanitizeUserMarkdown`, anti-phishing) — same as pastes; clickable in prod
where GFM linkify sees a dotted host.

## Search / RAG

PUBLIC notes go into BOTH community indexes gated on `visibility='public'`
(migration 00064, mirrors 00062 for pastes): `search_fts` (live triggers) +
`embed_outbox` (RAG, async; the loader in `internal/rag/repo.go` `KindNote`
re-applies the gate, so a private note's enqueued row resolves to a no-op
delete). `kind='note'` rendered in `internal/search` (URL `/c/{slug}/notes/{id}`,
📝 icon, "note" label).

PRIVATE notes are full-text searchable by their **author only**, via a SEPARATE
`note_private_fts` index (migration 00065) keyed by `author_id` — they never
enter `search_fts`. `search.Service.Search(ctx, communityID, viewerID, slug, …)`
queries it scoped to `(community_id, author_id=viewerID)` when viewerID is set;
the `/search` page passes the session user, the chat `/search` slash command
passes `""` (public-only). Codex-reviewed for the privacy boundary.

## Editor UX

The edit+preview zone collapses via a Hide/Edit toggle (FE-only `_note_edit`) so
an editor can read clean; the reader header always renders so the title shows
when collapsed. Live preview is OPT-IN (`_note_live`, default off) — the textarea
is `data-on:input__debounce.300ms="$_note_live && @post('…/preview')"` plus an
explicit "↻ Update preview" button. The per-line gutter "+" sits in each block's
OWN left padding (`#note-body > [data-nb]`, toggled via opacity+pointer-events),
NOT a negative offset — a negative offset put it outside the block box so moving
toward it ended `:hover` and hid it before a click.

## Collaborative editing (server-OT diff-sync)

A few mods/admins edit one note's markdown at once without losing data
(migrations 00066 `version`, 00067 `draft_body`). Server-mediated differential
sync — the single-writer SQLite DB is the sequencer:

- **Draft vs published.** Collab edits a shared **`draft_body`**; **`body`** (+
  `body_html`, FTS/RAG) is the *published* copy, updated only on **Save**. So
  in-progress edits never hit the public search index or the rendered reader, and
  Save never clobbers a concurrent editor (the draft is the one canonical the
  merge owns). The editor textarea shows `draft_body` (`toView.Body = DraftBody`);
  the reader renders published `body_html`.
- **Flow.** `note-collab.js` keeps a `shadow` (last canonical); on debounced input
  it diffs shadow→local, POSTs a diff-match-patch patch as the `note_patch` signal
  to `/sync`. `Repo.MergeBody` fuzzy-applies it onto `draft_body` in a tx, bumps
  `version`. The per-note `Bus` + NATS (`natsx.NoteSubject`) fan out; the
  **`/collab` SSE stream** pushes `draft_body`+`version` as `_note_canon`/`_note_ver`
  signals (PatchSignals — NOT html morph); the client merges into its textarea
  preserving the caret (`fcNoteCollabApply`). dmp patch text interops Go↔JS.
- **Save flushes then publishes.** `PostSave` sends the final `note_patch`;
  `Service.Save` merges it into the draft (no clobber), then sets `body = draft`,
  renders `body_html`, persists. A non-collab caller (no patch) sets the draft
  from `note_body` directly — last-writer-wins, fine single-editor.
- **Hardening (Codex):** `MergeBody` returns `ErrBadPatch` on a malformed patch
  (no write, no version bump → `/sync` 400s); `MaxBytesReader` caps `/sync`;
  `SyncBody`/`GetCollab` enforce community + `CanEdit` (a non-editor can't sync or
  even subscribe). `SetMaxOpenConns(1)` makes the read-modify-write atomic.
- **Remote carets (Phase 2).** `notes.Presence` (in-memory, per-note) tracks each
  editor's caret offset; the textarea reports `note_cursor` on input/keyup/click,
  `/sync` updates presence and always broadcasts (so cursor moves propagate). The
  `/collab` stream pushes the OTHER editors' carets as the `_note_cursors` signal
  (JSON string), and includes the canonical body only when `version` changed
  (cursor-only moves stay cheap). `note-cursors.js` renders a colored caret + name
  label per remote editor via the mirror-div caret-coordinates technique; colour
  is a stable hash of the editor id. Caret position is approximate (mirror
  technique) and best-effort under same-instant typing.

## CQRS shape (AGENTS §6b)

`notes.go` = types + Repo (SQL) + Service (write orchestration: render, token
mint, guards). `handler.go` = HTTP boundary; `readerData`/`readerDataSlug` is the
read model (load comments + annotate + map views) reused by GetPage, GetShared,
PostSave and every comment mutation.

<claude-mem-context>
# Recent Activity

<!-- This section is auto-generated by claude-mem. Edit content outside the tags. -->

*No recent activity*
</claude-mem-context>
