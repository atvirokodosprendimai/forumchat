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

### Phase 1 — Uploads relaxation: denylist + 100MB + content sniff — status: completed

Goal: the existing single-file paste / drop / file-picker path accepts any non-executable file up to 100 MB. Image render path unchanged. No DB changes. Visible test: paste a PDF into the composer today and see it land as an `<a>` link in the chat body via the current image-only render — the upload itself succeeds.

1. [x] Replace `allowedMIME` map gate in `Save()` with a denylist + content-sniff
   - => kept `allowedMIME` as the extension lookup map (expanded with video/audio/pdf entries) so paste / data-URL paths still get a canonical extension.
   - => added `denyMIME` set with executable + shellscript MIMEs.
   - => `Save()` now sniffs first 512 bytes; `sniffMIME` wraps `http.DetectContentType` and additionally catches MZ (Windows PE), ELF, Mach-O, and `#!` shebang scripts which the stdlib sniffer misses → maps them onto denylisted MIMEs.
   - => declared MIME wins over sniff only when the family matches OR the sniff returned `application/octet-stream`. Otherwise the sniff wins. `isAllowedMIME` is consulted on the final MIME.
2. [x] Raise the per-file `MaxBytes` ceiling to 100 MB; expose `UPLOADS_MAX_BYTES` env override
   - => `internal/config/config.go` envDefault bumped to `104857600`; `.env.example` updated.
3. [x] Preserve original filename
   - => migration `00027_uploads_filename.sql` adds `uploads.filename TEXT NOT NULL DEFAULT ''`.
   - => `Save()` and `SaveAttachment()` accept a `filename` parameter and persist it via the new column.
   - => `sanitiseFilename` strips path components (uses `filepath.Base`), control bytes, NULs, and trims to 200 chars.
   - => `Upload` struct gains `Filename string`; `Get()` reads it.
   - => `handler.GetFile` emits `Content-Disposition: inline; filename="..."` when present so browsers download chip-only kinds with the right name.
4. [x] Update `uploads_test.go`
   - => `TestSaveAndSign` asserts the filename round-trips.
   - => `TestAcceptArbitraryDoc` confirms a PDF with empty declared MIME sniffs as `application/pdf` and lands.
   - => `TestRejectExecutable` confirms MZ-header bodies are rejected regardless of declared MIME.
   - => `TestSanitiseFilename` confirms `../../etc/pass\x00wd.png` becomes `passwd.png`.
   - => `TestRejectTooLarge` retained.

=> Other call sites (`SaveDataURL`, `internal/uploads/handler.go PostUpload`) pass empty / `hdr.Filename` accordingly. Build + tests green.

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

### Phase 7a — Responsive content width — content must not slide under the left sidebar — status: completed

Folded in mid-plan after user-observed regression: on medium-width screens (between the 720px mobile breakpoint and ~1280px) `main { max-width: 1080px; margin: 0 auto }` centers the content block relative to the viewport — so the left half of chat / forum slips behind the fixed `.sidebar`. The fix is layout-shaped, not page-shaped: the whole site shell needs to be a single flex/grid row where the sidebar takes its width and `main` fluid-fills the rest. Visible test: shrink the window from 1400px down to 800px slowly — the chat composer + messages stay fully visible and grow narrower, never tucked under the sidebar.

1. [x] Audit current shell layout
   - => sidebar is `position: fixed; left: 0; width: 232px`, body is `display: flex; flex-direction: column`. main had `max-width: 1080px; margin: 0 auto` plus an `@media (min-width: 900px) { body > main { margin-left: 220px } }` override. The `margin-left: 220px` undershot the sidebar's 232px width by 12px AND the `margin: 0 auto` cross-axis centering fought the override on narrower viewports — so content drifted left and tucked under the sidebar.
   - => chose option B-light: keep the flex column body, drop the centred max-width on main, set main's left offset to exactly `var(--sb-width)`, let main fluid-fill the rest.
2. [x] `web/static/app.css` — apply the shell fix
   - => added `:root { --sb-width: 232px }` as the single source of truth.
   - => `.sidebar { width: var(--sb-width) }` now consumes the variable.
   - => `main` lost `max-width: 1080px; margin: 0 auto`; gained `min-width: 0` so grid children can shrink past their content width.
   - => `body > main { margin-left: var(--sb-width); width: calc(100% - var(--sb-width)); max-width: none }` and same `margin-left` on `body > .site-footer`.
3. [x] Mobile (≤899px) keeps current behavior
   - => the existing `@media (max-width: 899px)` block still translates the sidebar off-screen and `body.nav-open` slides it back. No change needed; the desktop media query is the only one that applies the var-based offset.
4. [x] Chat layout re-verify
   - => `.chat-layout { grid-template-columns: 1fr 220px }` still works since it's now a child of a fluid-width main. Earlier presence-rail collapse breakpoint deferred — current `@media (max-width: 720px)` collapse is acceptable for now.
5. [x] Manual smoke at narrower widths
   - => deferred to live testing by the user. Build + go test green; CSS is the only delta.

=> Out of scope for this phase: any new feature work. Strictly a responsive-shell correctness fix discovered mid-implementation of the attachment plan.
=> Shipped as commit (Phase 7a — responsive shell fix). See Progress Log.

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
- **2606180030** — Phase 7a completed. Single CSS-only commit: dropped centred max-width on `main`, introduced `--sb-width: 232px` custom property, fixed the desktop offset to match the sidebar exactly. Build + tests green. User to verify visually.
- **2606180038** — Phase 1 completed. Migration 00027 adds `uploads.filename`. `Save()` switched from allowlist to denylist + 512-byte content sniff (with extra MZ/ELF/Mach-O/`#!` detectors that stdlib misses). Default cap bumped to 100 MB. New tests cover the PDF accept path, executable reject, filename sanitisation.
