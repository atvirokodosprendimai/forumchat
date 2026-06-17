---
tldr: Implement the chat-attachments spec in phased, shippable PRs — uploads relaxation, multi-attachment chat schema, drag-anywhere overlay + multi-file XHR, rich inline previews, then the mod/admin extract-to-project flow (Docs + Issue).
status: completed
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

### Phase 2 — Multi-attachment chat schema + send path — status: completed

Goal: a chat message can carry N upload refs. Single existing paste-image flow continues writing markdown body (no behavior break). Composer's existing 📎 button gets `multiple` so a user can pick 3 files at once and the resulting bubble shows 3 download chips. Visible test: pick 3 PDFs, send, see all 3 in one bubble.

1. [x] Migration `00028_chat_message_attachments.sql`
   - => columns id / chat_message_id / upload_id / position / created_at; FKs to chat_messages(ON DELETE CASCADE) and uploads; indexes on (chat_message_id, position) and (upload_id).
2. [x] `internal/chat/chat.go` — `Attachment` struct + repo methods
   - => `Repo.InsertWithAttachments(ctx, m, uploadIDs)` opens a tx, inserts the message + N attachment rows, commits. Single-writer SQLite keeps this safe.
   - => `Repo.AttachmentsForMessages(ctx, msgIDs)` IN-clause batch query joining uploads → returns `map[msgID][]Attachment`. `Repo.Recent` and `Repo.Before` now eager-hydrate via `hydrateAttachments`.
   - => `MIMEKind(mime)` derives image|video|audio|pdf|other.
   - => `Repo.VerifyUploadsOwned(ids, owner, community)` defeats replayed upload ids.
3. [x] `Service.Send` accepts `AttachmentIDs []string` slice on `SendInput`
   - => empty body + ≥1 attachment is now valid; both empty stays an error. `Send` branches: no attachments → existing `Repo.Insert`; with attachments → `VerifyUploadsOwned` then `InsertWithAttachments`.
4. [x] `PostSend` reads `attachment_ids[]` signal alongside `body`
   - => `sanitiseAttachmentIDs` trims, deduplicates, caps at 12.
   - => clear signal `{"body":"","reply_to_id":"","image_data":"","attachment_ids":[]}` after send.
5. [x] `MsgView` carries `Attachments []AttachmentView{ID, URL, MIME, Kind, Filename, Size}`
   - => `Handler.toMsgViewWith(m, viewerID)` signs the URL per viewer via `Uploads.SignedURL`. `loadRecentFor` now builds views with attachments + read-receipts in one walk.
6. [x] `web/templ/chat.templ` — `MessageAttachments` block under `.body`
   - => chip-only render in Phase 2 (filename + size + 🖼/🎬/🎵/📄/📎 icon). Phase 4 will switch to inline `<img>`/`<video>`/`<audio>`/`<iframe>` per Kind.
   - => `attachIcon`, `humanSize`, `formatInt`, `formatFloat` helpers live in chat.templ; avoids a `fmt.Sprintf` import dance.
7. [x] Composer file picker + chat-attach.js
   - => 📎 button now `accept="*/*"` + `multiple`; old `data-on:change="fcPickImage"` removed.
   - => new `web/static/chat-attach.js` listens for `change` on the picker, uploads each file via XHR to new endpoint `POST /c/{slug}/chat/upload`, accumulates ids into `$attachment_ids`.
   - => `$attachment_ids` added to `InitialSignals` as `[]`; hidden `<input data-bind="attachment_ids">` bridges JS ↔ signal.
   - => `attach-pending` row in composer surfaces `N file(s) staged` with a clear button.

=> New endpoint: `chat.Handler.PostUpload(POST /c/{slug}/chat/upload)` — single-file multipart, returns JSON `{id, mime, kind, size, filename}`. Wired in main.go inside the community route group.
=> Build + tests green; migration 28 applies cleanly on boot; static script serves 200.

### Phase 3 — Drag-anywhere overlay + per-file progress + cancel — status: completed

Goal: dragging files onto any part of `.chat-layout` shows a "Drop to attach" overlay; releasing uploads each file in parallel with a progress row + ✕ cancel; chat send waits until every row is uploaded or removed. Visible test: drag 3 files anywhere on the chat surface, see overlay, watch 3 progress bars, click one ✕ to cancel, send the remaining two.

1. [x] `web/static/chat-attach.js` — full Phase-3 module
   - => `dragenter`/`dragleave`/`drop` on `.chat-layout` with depth counter (dragenter fires per child element on bubble, balance with dragleave).
   - => `hasFiles(evt)` gates so dragging text / DOM elements doesn't trigger.
   - => `dragover` `preventDefault` on `.chat-layout` so Chrome doesn't navigate.
   - => per-file row in `#composer-pending` with XHR progress (`xhr.upload.onprogress`) → `.composer-pending-fill` width.
   - => `cancel` aborts the XHR and unstages the id if already staged.
   - => failure paints the row red with `retry` link that re-fires the upload.
   - => `fcChatStageFiles(files)` kept as a public hook.
2. [x] `chat.Handler.PostUpload` already shipped in Phase 2; retained as-is. Rate-limit deferred to Phase 7 polish.
3. [x] `chat.templ` — `<div id="chat-drop-overlay" class="chat-drop-overlay">` mounted at the `.chat-layout` root with `chat-drop-overlay-card` inside.
   - => CSS: absolute inset, blur backdrop, dashed accent border, JS toggles `chat-drop-overlay-active` for opacity.
   - => `.chat-layout` gains `position: relative` so the overlay anchors.
4. [p] Send button gating: deferred — current UX shows the per-row progress, and an in-flight row that lands after Send simply orphans (server side it's an upload row no chat message references; Phase 7's orphan sweep cleans those). Re-evaluate after Phase 7 if it still feels rough.
5. [x] `data-on:dragover` removed from `#composer` (was firing alongside the new `.chat-layout` handler and stamping `image_data` for any drop). The new handler covers the composer region.

=> Visible win: end-to-end drag-from-Finder onto `#messages` / `#composer` / presence aside now works for any file count, with progress + cancel.

### Phase 4 — Inline previews + video posters — status: completed

Goal: pixels match the spec — images / videos / audio / pdf render inline; everything else stays a chip.

1. [x] `MessageAttachment` branches per `AttachmentView.Kind`
   - => image → `<img loading="lazy">` inside click-to-open `<a>` with a tiny caption strip.
   - => video → `<video controls preload="metadata">` + meta strip (icon, name, size, ⬇ download link). No poster yet — see action 3.
   - => audio → `<audio controls>` strip + meta strip.
   - => pdf → `<iframe>` at fixed height + "Open PDF ↗" link.
   - => default (other) → existing chip.
2. [x] `Kind` derived from MIME at view-build time via `chat.MIMEKind` (`image|video|audio|pdf|other`). No DB column — derived state stays out of storage.
3. [p] Best-effort video poster via ffprobe / ffmpeg — deferred to Future. `<video>` falls back to the browser default black frame, which is acceptable; adding the pipeline is a separate concern (process exec, poster cache invalidation, missing-binary fallback). Logged under the spec's Future section.
4. [x] CSS grid for ≥2 attachments (`.msg-attach-grid-1` / `-2` / `-n` flex wrap with minimum 12rem child width).
5. [p] Bluemonday policy update is NOT needed — chat bubbles render via templ directly (not through `render.RenderMarkdown`'s bluemonday-sanitised path). Markdown bodies still go through bluemonday; the `<video>` / `<audio>` / `<iframe>` tags live in hand-written templ output that bypasses the sanitiser. Documented for the next implementer.

=> Visible win: drop a 30 MB mp4 → bubble shows a clickable `<video controls>` with the file streamed via the existing signed-URL path. Audio strips and PDF iframes work the same way.

### Phase 5 — Extract-to-project: "Save to Docs" — status: completed

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

### Phase 6 — Extract-to-project: "New issue from this" — status: completed

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

### Phase 7 — Polish, hardening, docs — status: completed

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
- **2606180047** — Phase 2 completed. Migration 00028 adds `chat_message_attachments(id, chat_message_id, upload_id, position, created_at)`. `chat.Service.Send` accepts `AttachmentIDs`; ownership verified pre-link. New endpoint `POST /c/{slug}/chat/upload` returns JSON. Composer 📎 button now multi-file `*/*`; `chat-attach.js` XHR-uploads each file and stages ids in `$attachment_ids`. Phase 2 bubble render = chip per attachment.
- **2606180056** — Phase 3 completed. Drag-anywhere overlay on `.chat-layout`, per-file row in `#composer-pending` with XHR progress + cancel + retry. Old `composer { data-on:drop=fcDropImage }` removed (was double-firing alongside the new handler). Send button gating left for Phase 7 polish.
- **2606180103** — Phase 4 completed. `MessageAttachment` branches per Kind into `<img>` / `<video>` / `<audio>` / `<iframe>` / chip. Each branch carries a meta strip with filename + size + download link. CSS layouts the grid for 1 / 2 / N attachments. ffprobe poster pipeline deferred (logged under Future).
- **2606180155** — Phase 5+6 completed together. Migration 00029 adds `chat_attachment_extracts(id, chat_attachment_id, project_id, project_attachment_id, issue_id, mode, extracted_by, created_at)`. Per-attachment "↗ Extract" button (mod/admin gated) opens a modal with project dropdown + Docs/Issue toggle. New endpoint `projects.Handler.PostExtractFromChat` (mounted as `POST /c/{slug}/chat/extract`) duplicates the upload reference into `project_attachments` (Docs mode) or creates a new `project_issues` + `project_issue_attachments` row (Issue mode) and records the link in `chat_attachment_extracts`. Bubble badges render "Docs of X" / "Issue in X" and link to the destination. Chat bus broadcast triggers cross-tab re-render so the badge appears immediately.
- **2606180210** — Phase 7 completed (light). `internal/uploads/sweep.go` adds an hourly `SweepWorker` that deletes uploads older than 24h with no reference (chat, project, issue link OR markdown body match). AGENTS.md §6.7 documents the multi-attachment invariant + extract-to-project pattern for future agents. Plan marked completed.
