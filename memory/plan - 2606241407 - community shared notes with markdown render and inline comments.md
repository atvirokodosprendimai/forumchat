---
status: completed
created: 2026-06-24
claim: community-wide shared "iNotes" — markdown source / HTML render, visibility split, share-to-channel, inline line+selection comments
---

# Plan — community shared notes (iNotes)

## Context

Community-wide shared notes, macOS-Notes style. Edit/create in **markdown** with
a **HTML preview**; the stored note renders as sanitized HTML. A note is either
**public** (listed community-wide) or **private** (not listed; readable by an
unguessable capability-token link). Notes can be **shared to a channel** (drops
the note URL into chat). Rendered HTML supports **inline comments** anchored to a
line (rendered block) or a selected-text range.

Closest existing analog (clone target): `internal/pastes` — per-community
markdown→HTML on a dedicated page + share-to-channel via a `PostToChat` closure
wired in `cmd/app/main.go:994` (no chat import cycle). Notes differ: a *listed
collection*, *repeatedly editable*, *visibility split*, *inline comments*.

Reused seams / references:
- Render pipeline: `render.RenderMarkdown` (`internal/render/markdown.go:335`) —
  goldmark + bluemonday, the one sanitizer. Do not add another.
- Share-to-channel closure pattern: `pastesHandler.PostToChat` (`main.go:994`)
  and `pasteMessage` absolute-URL builder (`internal/pastes/handler.go:195`).
  Memory: [[project_webhook_relay_absolute_urls]] — relayed body must be absolute.
- SSE: `render.NewSSE` not raw `datastar.NewSSE` (memory:
  [[project_render_newsse_not_datastar]]).
- Public token route precedent: data-export landing/download mounted at router
  root, token-gated, identity-optional (`main.go:2000-2001`, AGENTS §5h).
- Auth: `auth.Loader` is global (`main.go:226`) → identity available on every
  route; `RequireApproved` gates the community group (`main.go:1679`).
- UI: PageHeader/`.toolbar`, index-list vs single-card two-pattern, top-bar pill
  per feature (memory: [[project_ui_design_system]], [[project_nav_topbar_community]]).
- FTS+RAG dual-index trigger shape: `00062_search_pastes.sql` (gate on
  visibility, mirror for `kind='note'`).
- Datastar EDA for many-trigger→one-listener (AGENTS §4.12); string-signal-from-JS
  via expr not bool hidden input (§4.6); leaf-package view models (§4.13).

## Decisions (locked with user)

1. **Edit rights** — author OR `Role.AtLeast(RoleModerator)` edits; everyone else
   reads + comments. Mod/admin can delete.
2. **Private link** — unguessable capability token (`share_token`, 32B). Readable
   without login via a dedicated public read-only route; the note stays unlisted.
   *Deviation from data-export's two-step:* a single read-only GET is sufficient —
   the shared note IS the thing meant to be read (a prefetch = a view, not bulk
   exfil of unrelated tenant data). Documented here, not silently chosen.
3. **Inline comments** — anchor = `{block_index, quote?}` (quote `""` = whole-line
   comment, else selected-text snippet). Add: any approved member (anon token
   readers cannot). Resolve/delete: author OR mod. Edit drift → comment whose
   block_index no longer resolves is shown "orphaned" in the margin.

## Phases

### Phase 0 — schema + domain — status: completed
1. [ ] Migration `00063_notes.sql`: `notes` (id, community_id, author_id, title,
   body, body_html, visibility `'public'|'private'` default private, share_token,
   created_at, updated_at; idx community; unique partial idx share_token) +
   `note_comments` (id, note_id, community_id, author_id, block_index, quote,
   body, body_html, resolved_at, created_at; idx note).
   - => verify: `make build` boots & migrates clean.
2. [ ] `internal/notes/notes.go`: `Note`, `Comment` structs; `Repo` (Create,
   ByID, ByShareToken, ListPublic, ListByAuthor, Update, Delete; comment CRUD,
   ListComments, ResolveComment, DeleteComment); `Service` (CreateDraft, Save
   [render+visibility+token mint], AddComment [render], guards).
   - => `CanEdit(note, identity)` helper = author || Role.AtLeast(moderator) ||
     IsSuperAdmin. One authority, reused by handler.
3. [ ] `internal/notes/service_test.go`: create→save→public/private,
   token read gate, CanEdit matrix, add/resolve comment. `t.TempDir()` DB.
   - => verify: `go test ./internal/notes/...`.

### Phase 1 — index + editor + reader (visible result) — status: completed
4. [ ] `web/templ/notes.templ`: leaf view models (`NoteView`, `NoteCommentView`,
   `NotesPageData`, `NotePageData`); `NotesIndex` (public list + my notes);
   `NoteEditor` (title, markdown textarea, visibility toggle, live `#note-preview`,
   Save); `NoteReader` (rendered article); `NoteError`. `templ generate`.
5. [ ] `internal/notes/handler.go`: viewer/cid helpers (clone pastes:28-66);
   GetIndex, PostNew→redirect editor, GetPage (editor if CanEdit else
   member-reader), PostSave, PostPreview (debounced live preview), PostDelete.
6. [ ] Wire in `main.go` after pastes block (~979/1715): repo+service+handler,
   routes in authed `/c/{slug}` group. Add top-bar **Notes** pill
   ([[project_nav_topbar_community]]).
   - => verify: run app, create note, edit markdown, see live HTML preview, save,
     reopen, list — screenshot ([[feedback_ux_first]]).

### Phase 2 — share-to-channel + public token reader — status: completed
7. [ ] `PostToChat` closure for notes (clone `main.go:994`); `noteMessage`
   absolute-URL builder (clone `handler.go:195`). PostShare: public note → member
   URL; private note → `/notes/{id}/shared?token=` absolute URL. Channel picker
   signal (reuse pattern).
8. [ ] Public read-only route `GET /c/{slug}/notes/{id}/shared` mounted with
   community-slug resolution but NOT RequireApproved (mirror exports root mount).
   Token match → `NoteReader` (anon ok, no comment composer); miss → flat 404.
   - => verify: share public + private; open private token URL logged-out → reads;
     bad/no token → 404.

### Phase 3 — inline comments (line + selection) — status: completed
9. [ ] `render.AnnotateBlocks(html) (html, n)` — x/net/html (already a dep via
   bluemonday) tags each top-level block `data-nb="<i>"`; display-time, cheap.
10. [ ] Reader/editor render under one stable root `#note-reader` (article +
    margin comments + per-block badge), fat-morphed on any comment change
    (§4.7 live-morph-via-stable-id). Margin lists comments by block; orphaned
    (block_index ≥ n) grouped at end.
11. [ ] `web/static/note.js`: `fcNoteSelection('#note-body')` → `{block, quote}`
    from `window.getSelection`. Gutter "💬+" per block + floating selection button
    both `dispatchEvent('fc:note-comment', {block, quote})` (EDA §4.12); one
    consumer sets `note_c_block`/`note_c_quote` (string signals via expr, §4.6)
    + opens composer.
12. [ ] PostComment (add), PostResolveComment, PostDeleteComment (author/mod
    gate); CSS in `app.css` for gutter, margin column, badge, resolved state,
    selection button ([[feedback_ux_first]] — every class styled, mobile-safe).
    - => verify: line comment + selection comment, resolve, delete; edit note to
      shift blocks → orphaned shown.

### Phase 4 — search/RAG index + docs — status: completed
13. [ ] Migration `00064_search_notes.sql`: FTS5 + embed_outbox triggers for
    `kind='note'`, gated `visibility='public'` (clone `00062_search_pastes.sql`).
    Wire the rag loader case if the loader switches on kind.
    - => verify: post a public note, search finds it; private note absent.
14. [ ] `internal/notes/CLAUDE.md` + AGENTS.md section; update plan progress +
    memory palace diary.

## Verification (done = all)
- `make build` + `make gen` clean; `go test ./...` green.
- Manual: create→edit(md+preview)→save; public listed, private unlisted; private
  token link reads logged-out; share-to-channel posts clickable absolute link;
  line + selection comments add/resolve/delete; orphan-on-edit; search finds
  public note only.
- Codex review gate (handlers parse untrusted token + selection input → §ask).

## Progress Log
- 2606241407 — plan created; sources reconciled (no conflict): pastes is the
  template, memory reinforces. Decisions locked via AskUserQuestion.
- 2606241420 — Phase 0 done: migration 00063 (notes + note_comments), domain
  Repo+Service, 7 tests green. CanEdit=author|mod|superadmin one authority.
- 2606241500 — Phase 3 done: inline comments. render.AnnotateBlocks (data-nb),
  note.js (selection btn + gutter + badges + jump via one fc:note-comment EDA
  event), comments rail, composer, resolve/delete, orphan-on-edit. Verified e2e.
  Fixes folded: MutationObserver infinite loop (detach during paint), .link.danger
  red-on-red. Codex earlier pass (Phase 2) → community-guard + body-cap fixes.
- 2606241510 — Phase 4 done: migration 00064 (FTS5 + RAG triggers gated
  visibility='public'), rag KindNote loader, search kind='note' rendering, docs
  (internal/notes/CLAUDE.md + AGENTS §5j). Verified: public note in search,
  private excluded. ALL PHASES COMPLETE. Branch task/community-notes pushed;
  not yet merged to main (awaiting user).
- 2606241432 — Phase 2 done: share dialog (channel picker + copy link) posts
  note URL to chat via PostToChat; token minted at draft-create so the copy link
  is correct pre-save; public /n/{token} read-only reader (anon-readable, miss →
  generic "unavailable"). Verified e2e (5 shots incl. logged-out token read).
  Codex review folded in: HIGH cross-community Save guard + MEDIUM PostShare
  body cap; regression test added. Note: chat link shows the bare URL (shared
  sanitizeUserMarkdown flattens [label](url) anti-phishing — same as pastes;
  clickable in prod where GFM linkify sees a dotted host).
- 2606241425 — Phase 1 done: Notes topnav pill; index (shared + my notes) +
  editor (title/visibility/debounced live preview/save morphs reader in place/
  delete) + reader. Verified via Playwright (4 screenshots). Fixed grid overflow
  (minmax(0,1fr)) + snippet markdown-marker strip after seeing the index shot.
