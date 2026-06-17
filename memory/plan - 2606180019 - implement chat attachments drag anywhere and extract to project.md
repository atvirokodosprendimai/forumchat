---
tldr: Implement the chat-attachments spec in phased, shippable PRs — uploads relaxation, multi-attachment chat schema, drag-anywhere overlay + multi-file XHR, rich inline previews, then the mod/admin extract-to-project flow (Docs + Issue).
status: active
---

# Plan: Chat attachments — drag anywhere, any file, extract to project

## Context

- Spec: [[spec - chat-attachments - drag-anywhere-multi-mime-extract-to-project]]
- Related specs touched during extract:
  - [[spec - projects - per-community-collaborative-projects]] — Docs tab target.
  - [[spec - project-issues - per-project-issues-with-guest-share-links]] — Issue target.
  - [[spec - forumchat - community web app with realtime chat and forum threads]] — chat fan-out + message schema.
- Decisions baked into the spec (from /eidos:spec Q&A on 2026-06-17):
  - MIME: denylist + content sniff (allow everything except executables).
  - Per-file cap: 100 MB. No chunked / resumable upload in v1.
  - Drop target: whole `.chat-layout`.
  - Multi-file drop → one bubble with N attachments.
  - Inline preview for image / video / audio / pdf; chip otherwise.
  - Per-file XHR progress bar + cancel.
  - Extract: mod + admin; both "Save to Docs" and "New issue from this".
- Existing code to extend, not rewrite:
  - `internal/uploads/` — already does sha + signing + dedupe; allowlist lives here.
  - `internal/chat/chat.go,handler.go` — fat-morph pattern stays; only `Insert` and read paths grow new joins.
  - `internal/projects/` — `ListAttachments`, `Repo.InsertAttachment`, issue + issue-attachment patterns reused as-is.
  - `web/static/paste.js`, `web/static/projects.js` — XHR + dropzone patterns to mirror.

## Phases

### Phase 1 — Uploads relaxation: denylist + 100MB + content sniff — status: open

Goal: the existing single-file paste / drop / file-picker path accepts any non-executable file up to 100 MB. Image render path unchanged. No DB changes. Visible test: paste a PDF into the composer today and see it land as an `<a>` link in the chat body via the current image-only render — the upload itself succeeds.

1. [ ] Replace `allowedMIME` map in `internal/uploads/uploads.go` with a denylist helper + content-sniff
   - keep `extFor(mime)` for download filename, derived from a wider extension map
   - reject `application/x-msdownload`, `application/x-msdos-program`, `application/x-sh`, `application/x-bsh`, `application/x-csh`, `application/x-perl`, `application/x-python`, `application/x-php`, `text/x-shellscript`
   - sniff via `http.DetectContentType` on first 512 bytes; trust sniff over client-declared MIME
2. [ ] Raise the per-file `MaxBytes` ceiling to 100 MB; expose `UPLOADS_MAX_BYTES` env override
   - default config bumps from 5 MB → 100 MB; document in `.env.example`
3. [ ] Preserve original filename
   - new column `uploads.filename TEXT NOT NULL DEFAULT ''`
   - migration `00027_uploads_filename.sql`
   - `Save()` sanitises (`filepath.Base`, strips path traversal + control bytes), persists
   - `Open()` returns it; signed-URL handler emits `Content-Disposition` for chip-only kinds
4. [ ] Update `uploads_test.go` — denylist round-trip, sniff overrides bad client MIME, 100 MB boundary

### Phase 2 — Multi-attachment chat schema + send path — status: open

Goal: a chat message can carry N upload refs. Single existing paste-image flow continues writing markdown body (no behavior break). Composer's existing 📎 button gets `multiple` so a user can pick 3 files at once and the resulting bubble shows 3 download chips. Visible test: pick 3 PDFs, send, see all 3 in one bubble.

1. [ ] Migration `00028_chat_message_attachments.sql`
   - `id TEXT PK, chat_message_id TEXT FK chat_messages(id) ON DELETE CASCADE, upload_id TEXT FK uploads(id), position INTEGER DEFAULT 0, created_at INTEGER`
   - index `(chat_message_id, position)` for ordered fetch
2. [ ] `internal/chat/chat.go` — `Attachment` struct + repo methods
   - `Repo.InsertAttachments(ctx, tx, msgID string, atts []Attachment)` — batch insert in the same tx as `chat_messages` insert
   - extend `Repo.listBefore` / `Repo.ByID` to eager-load attachments per message (one extra query, IN-clause keyed by msg ids)
3. [ ] `Service.Send` accepts `AttachmentIDs []string` slice on `SendInput`; wraps insert + attachments in a tx
   - validates each upload row belongs to the sender + community
   - empty body + ≥1 attachment is valid; both empty stays a no-op
4. [ ] `PostSend` reads `attachment_ids[]` signal alongside `body`
   - existing `image_data` paste path keeps its current markdown-rewrite shape; spec leaves room to migrate later
   - clear `attachment_ids` signal after send
5. [ ] `MsgView` carries `Attachments []AttachmentView{ID, MIME, Kind, Filename, Size, SignedURL}`; `toMsgView` populates from the eager load
6. [ ] `web/templ/chat.templ` — `MessageAttachments(atts []AttachmentView)` block rendered under `.body`
   - Phase 2 just renders a generic chip per attachment (filename + size + download). Inline previews land in Phase 4.
7. [ ] Composer file-picker `accept="*/*"` + `multiple`. Submit drains pending uploads first.
   - introduce a `$attachment_ids` JSON-array signal in `InitialSignals`
   - basic per-file XHR uploader inlined in `chat-attach.js` (no progress UI yet)

=> Output of phase 2: working multi-file send via the picker. Drag-from-outside lands a single file via existing `paste.js` drop handler. The drag-anywhere overlay + parallel uploads + progress UI come in Phase 3.

### Phase 3 — Drag-anywhere overlay + per-file progress + cancel — status: open

Goal: dragging files onto any part of `.chat-layout` shows a "Drop to attach" overlay; releasing uploads each file in parallel with a progress row + ✕ cancel; chat send waits until every row is uploaded or removed. Visible test: drag 3 files anywhere on the chat surface, see overlay, watch 3 progress bars, click one ✕ to cancel, send the remaining two.

1. [ ] `web/static/chat-attach.js` — new module replacing the inline phase-2 uploader
   - `dragenter` (with `DataTransfer.types.includes('Files')`) → set `$_chat_drop_active = true`
   - `dragleave` / `drop` → reset
   - drop handler uploads each file via `XMLHttpRequest` to `POST /c/{slug}/chat/upload` (multipart, single file per request — keeps progress events clean)
   - per-file row in `#composer-pending` shows thumbnail (object URL) or MIME icon, filename, size, progress bar, ✕ cancel button
   - failed upload turns row red with a retry link
2. [ ] `chat.Handler.PostUpload(w, r)` — multipart endpoint, single file
   - returns JSON `{upload_id, mime, kind, size, filename}`
   - rate-limit per-user via existing `httprate` middleware
3. [ ] `chat.templ` — full-cover `<div class="chat-drop-overlay" data-show="$_chat_drop_active">Drop to attach</div>` mounted at the `.chat-layout` root
   - CSS: `pointer-events: none` while hidden; `position: absolute; inset: 0; backdrop-filter: blur(2px)` while visible
4. [ ] Composer Send button + Enter listener gated on `$attachment_ids.length === pending_rows`
5. [ ] `data-on:dragover` on `.chat-layout` calls `evt.preventDefault()` so Chrome doesn't navigate when files drop outside the overlay

=> Visible win: end-to-end drag-from-Finder onto `#messages` works for any file count, with progress + cancel.

### Phase 4 — Inline previews + video posters — status: open

Goal: pixels match the spec — images / videos / audio / pdf render inline; everything else stays a chip.

1. [ ] `MessageAttachments` templ branches per `AttachmentView.Kind`
   - `image` → `<img loading="lazy" max-height: 280px>` inside a click-to-open `<a target="_blank">`
   - `video` → `<video controls preload="metadata" poster="...">`
   - `audio` → `<audio controls>`
   - `pdf` → `<iframe>` first-page preview + "Open PDF" link
   - `other` → existing chip
2. [ ] `internal/uploads` derives `Kind` from MIME at save time (column or computed): `image|video|audio|pdf|other`
3. [ ] Best-effort video poster via `ffprobe` + `ffmpeg`
   - detect `ffmpeg` in `$PATH` once at boot; gate poster generation on availability
   - poster path = `<sha>_poster.jpg`; signed-URL accessible via existing `/uploads/{id}/poster` route
   - skip silently when ffmpeg missing — `<video>` falls back to its default black frame
4. [ ] CSS: grid layout for ≥2 attachments (`.msg-attach-grid` — 1×1, 1×2, 2×2, 3×1 patterns by count)
5. [ ] Render-side bluemonday policy update — allow `<video>`, `<audio>`, `<iframe sandbox>` for chat templ output only (do not touch markdown render)

=> Visible win: drop a 30 MB mp4 → bubble shows a clickable `<video>` with poster.

### Phase 5 — Extract-to-project: "Save to Docs" — status: open

Goal: mod / admin sees an "Extract to project" item in the per-attachment menu. Picking a project + "Save to Docs" duplicates the upload reference (no file copy) into `project_attachments`. Bubble shows a "↗ in project X" badge afterward. Visible test: as a mod, attach a PDF in chat, extract → see it under the project's Docs tab.

1. [ ] `web/templ/chat.templ` — add "Extract to project" item inside `details.msg-menu`
   - gated on `viewer.Role >= moderator`
   - opens a small modal (datastar dialog) listing the viewer's joined projects
2. [ ] New endpoint `POST /c/{slug}/chat/extract` (mod/admin only)
   - signals: `{attachment_id, project_id, mode: "docs"}`
   - validates role + viewer membership of target project
   - `projects.Repo.InsertAttachment` with `upload_id` ref + category `chat-extract`
   - SSE response: PatchElementTempl that re-renders the attachment row with the badge
3. [ ] New table `chat_attachment_extracts` (msg_attachment_id, project_id?, issue_id?, kind, created_at) so the badge is rendered consistently on reload
   - migration `00029_chat_attachment_extracts.sql`
4. [ ] Eager-load `Extracts []ExtractRef` per attachment in `MsgView` so the read path stays cheap

### Phase 6 — Extract-to-project: "New issue from this" — status: open

Goal: second extract path. Mod / admin picks a project, the modal expands into a tiny issue composer (subject prefilled from filename, body empty), submit creates a new `project_issues` + `project_issue_attachments` row, response redirects to the new issue page. Visible test: extract a PDF as "new issue" → land on the issue with the file attached and the filename as title.

1. [ ] Extend the extract endpoint to accept `mode: "issue"` + optional `{title, body}`
   - default `title = strings.TrimSuffix(filename, ext)`
   - `projects.Service.CreateIssue` + `InsertIssueAttachment` in one tx
   - SSE: `sse.Redirect("/c/{slug}/projects/{pid}/issues/{iid}")`
2. [ ] Modal UI — mode toggle (Docs / Issue), shows extra fields only when mode = issue
3. [ ] Badge text branches: "↗ Docs of X" vs "↗ Issue #N of X"

### Phase 7a — Responsive content width — content must not slide under the left sidebar — status: open

Folded in mid-plan after user-observed regression: on medium-width screens (between the 720px mobile breakpoint and ~1280px) `main { max-width: 1080px; margin: 0 auto }` centers the content block relative to the viewport — so the left half of chat / forum slips behind the fixed `.sidebar`. The fix is layout-shaped, not page-shaped: the whole site shell needs to be a single flex/grid row where the sidebar takes its width and `main` fluid-fills the rest. Visible test: shrink the window from 1400px down to 800px slowly — the chat composer + messages stay fully visible and grow narrower, never tucked under the sidebar.

1. [ ] Audit current shell layout
   - `body` is currently `display: flex`-or-default with `aside.sidebar` + `main` siblings; sidebar likely `position: fixed` or floated
   - confirm the exact rule that lets `main` overlap the sidebar in the 720–1280px band
   - decide between two fixes:
     - **A. CSS Grid shell**: `body > { grid-template-columns: var(--sb-width) 1fr }` — clean, the sidebar takes the column, `main` fluid-fills.
     - **B. Flex shell**: `body { display: flex }`, sidebar fixed-width, `main { flex: 1 1 0; min-width: 0 }` — simpler, same result.
   - => pick during the action; record under `=>` notes
2. [ ] `web/static/app.css` — apply the chosen shell
   - drop `main { max-width: 1080px; margin: 0 auto }` and replace with a `max-width: 1080px` on inner `section` / `.chat-layout` / `.forum-list` where centered content is still desired
   - sidebar gets a CSS custom property `--sb-width: 240px` so the grid/flex math is one line to tweak later
   - `main { min-width: 0 }` so grid children don't blow out the column on long unbreakable lines (codeblocks, long URLs)
3. [ ] Mobile (≤720px) keeps current behavior — sidebar collapses into the hamburger drawer (`.topbar-hamburger`), `main` becomes full-width
   - verify the existing `@media (max-width: 720px)` block still wins
4. [ ] Chat layout (`.chat-layout { grid-template-columns: 1fr 220px }`) — re-verify after the shell change
   - on narrow desktops the presence rail may need an earlier collapse breakpoint (e.g. ≤960px → single column, presence moves to the FAB drawer that already exists)
5. [ ] Manual smoke at 1440 / 1280 / 1100 / 960 / 800 / 600 / 360 px viewport widths
   - record any rough edges as `=>` notes; small fixes land in the same commit, larger ones become new actions

=> Out of scope for this phase: any new feature work. Strictly a responsive-shell correctness fix discovered mid-implementation of the attachment plan.

### Phase 7 — Polish, hardening, docs — status: open

Goal: small finishing items so the feature feels shipped.

1. [ ] Reject orphan upload rows on a daily sweep — uploads with no attachment / no markdown reference older than 24 h get deleted (`internal/uploads/sweep.go`)
2. [ ] Error fragments for the composer rows (too-large, denied MIME, network drop) — replace silent failures with a red row + actionable text
3. [ ] Accessibility — drop overlay has `role="region" aria-label="File drop area"`; cancel button keyboard accessible
4. [ ] Update `AGENTS.md` § 6 with the new attachment shape so future agents don't break the multi-file invariant
5. [ ] Update the `CLAUDE.md` checklist of common errors with the "don't expand allowlist back to images-only" warning
6. [ ] `/eidos:observe` after each prior phase landed — fold any P*-O* fixes back into a new phase if needed

## Verification

- All 11 scenarios in the spec's `## Verification` section pass manually.
- `make test` green; new unit tests for:
  - `uploads`: denylist round-trip, sniff overrides client MIME, 100 MB boundary, filename sanitisation.
  - `chat`: send with attachment_ids creates rows + atts in one tx; `loadRecent` eager-loads atts.
  - `projects`: extract endpoint duplicates upload ref without copying bytes.
- Two browsers, two users: drop a 30 MB video → other tab sees it in the fat-morph window with poster + controls.
- Mobile Safari smoke: drop from Files.app → progress visible → send → other devices see the bubble.

## Adjustments

- **2606180024** — added Phase 7a (responsive content width). User reported chat/forum content sliding under the left nav on narrower viewports. Layout-shell fix, not feature work, but it's blocking real-world testing of the attachment changes so it goes ahead of Phase 7 polish.

## Progress Log

- **2606180019** — plan created from [[spec - chat-attachments - drag-anywhere-multi-mime-extract-to-project]] after /eidos:spec Q&A on 2026-06-17. Phases 1–7 drafted. Status = active.
- **2606180024** — Phase 7a inserted to address responsive-shell regression observed during testing.
