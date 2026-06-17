---
tldr: Chat accepts any file (image / video / audio / doc / arbitrary) via drag-anywhere on the chat layout — multi-file drops become one bubble with many attachments, inline-previewed where possible, and admins / mods can extract any attachment into a community project as a Docs entry or as a new issue.
---

# Chat attachments — drag anywhere, any file, extract to project

Today chat only accepts a single pasted/dropped image into the composer's hidden input, via `web/static/paste.js`. Anything else is rejected by the uploads MIME allowlist. This spec opens chat to arbitrary files, makes the drop target the whole chat layout (not just the composer), groups multi-file drops into a single bubble, and adds an admin / mod affordance to extract an attachment into a community project — either as a project doc or as a new issue.

## Target

- Members want to share videos, PDFs, slide decks, audio clips, and "just this random file" without leaving chat for Drive / Dropbox / email.
- Dragging onto the small composer area is fiddly — the whole chat surface should accept a drop.
- Multi-file drops shouldn't shred the conversation into N adjacent bubbles.
- Admins / mods often want to promote a "good doc someone posted in chat" into a project's permanent Docs tab, or open an issue from a screenshot / PDF.

## Behaviour

### MIME scope

- The uploads allowlist expands from images-only to **any file**:
  - **Inline-preview kinds** — images (jpg/png/gif/webp), videos (mp4/webm/mov/quicktime), audio (mp3/m4a/wav/ogg), pdf.
  - **Chip-only kinds** — docx / xlsx / pptx / odt / zip / txt / csv / json / md / source files / anything else.
- Server determines the MIME via content sniff (`http.DetectContentType` on the first 512 bytes), not from the client-declared header — the client value is hint-only.
- Filename is preserved verbatim (after sanitising path traversal + control bytes) so PDFs/docs land with their human name on download.
- Per-file size cap: **100 MB**. Chat send is rejected when total request body exceeds `cap × file_count`, capped at a hard ceiling so the server doesn't allocate gigabytes from a malicious drop.

### Drop target

- The drop zone is the entire `.chat-layout` element — `#messages`, the presence aside, the composer, everything inside.
- During a drag (`dragenter` carrying files): the layout shows a full-cover overlay reading "Drop to attach" with a subtle border. The overlay listens for `dragleave` / `drop` to hide.
- The composer's existing paste-image flow remains as-is — Ctrl+V still works.
- The 📎 attach button in the composer still triggers a file picker, now with `accept="*/*"` and `multiple`.

### Multi-file drop → one bubble

- Dropping N files (or pasting N image clipboard items) produces a single `chat_messages` row with N `chat_message_attachments` rows. See [[claim - chat-message-many-attachments-schema]].
- The bubble renders a grid (1 × 1 for a single image, 2 × 2 / 3 × 1 for more) of previews — images / videos / audio inline, everything else as a download chip with filename + size + a MIME-typed icon.
- The composer textarea body, if any, is rendered above the attachment grid in the same bubble.

### Inline preview rules

- **Image** — `<img>` lazy-loaded, max ~280 px tall in the bubble, click opens full size.
- **Video** — `<video controls preload="metadata">` with a poster pulled from the first frame at upload time when ffprobe is available (graceful fallback otherwise).
- **Audio** — `<audio controls>` thin strip.
- **PDF** — `<iframe>` first-page preview at fixed height with "Open PDF" link below.
- **Everything else** — chip: 📄 filename · 1.2 MB · download arrow.

### Upload progress + cancel

- The composer shows one row per in-flight upload: small thumbnail (or MIME icon), filename, per-file progress bar (`XMLHttpRequest` upload `onprogress`), and an ✕ cancel button that aborts the request and removes the row.
- A failed upload turns its row red with a "retry" link; chat send is blocked until all rows are either uploaded or removed.

### Extract to project (admin + mod)

- Each rendered attachment on someone else's message — and on own messages too — exposes an "Extract to project" item inside the existing `details.msg-menu`. Members do NOT see it. Permission gate: `viewer.Role >= moderator`.
- The submenu offers **two paths**:
  1. **Save to project Docs** — picks a target project from a dropdown of the viewer's joined projects; on submit the attachment is duplicated into `project_attachments` (re-using the existing uploads row by ref, no file copy), category defaulting to a `chat-extracts` label.
  2. **New issue from this attachment** — picks a project; opens a tiny composer (subject prefilled from filename, body empty) and on submit creates a `project_issues` row + a single `project_issue_attachments` row pointing at the same upload. Redirects to the new issue.
- Original chat message + bubble are unchanged. The attachment row gets a small "↗ in project X" badge after a successful extract — anyone seeing the bubble later knows it has been filed.

### Permissions recap

| Action | Member | Moderator | Admin |
|---|---|---|---|
| Drop / paste / pick files (any MIME, ≤100MB each) | ✓ | ✓ | ✓ |
| See "Extract to project" submenu | ✗ | ✓ | ✓ |
| Choose any project to extract into | — | only joined | any in community |

## Design

### Storage shape

- New table `chat_message_attachments`:
  - `id TEXT PK`
  - `chat_message_id TEXT NOT NULL` → FK `chat_messages(id)` ON DELETE CASCADE
  - `upload_id TEXT NOT NULL` → FK `uploads(id)`
  - `position INTEGER NOT NULL DEFAULT 0` (display order within the bubble)
  - `created_at INTEGER NOT NULL`
- `uploads` table gains nothing — its `mime` column is already opaque text, the allowlist was a code-side check.
- Code-side uploads guard relaxes: replace the `allowedMIME` map with a **denylist** (executables, scripts that would run if served — `application/x-msdownload`, `application/x-sh`, etc.) + a hard 100MB cap. See [[claim - chat-uploads-denylist-over-allowlist]].

### Endpoints

- `POST /c/{slug}/chat/upload` — multipart, one or more `files[]` parts. Returns JSON `[{upload_id, mime, size, name, kind}]`. Used by the composer's progress-tracked XHR.
- `POST /c/{slug}/chat/send` — body now accepts an additional `attachment_ids[]` JSON array. Server creates the message + N attachment rows in one transaction. Rejects if any attachment_id is owned by another user (cross-user replay).
- `POST /c/{slug}/chat/extract` — admin/mod only. Body: `{attachment_id, project_id, mode: "docs" | "issue", title?, body?}`. Server validates role, validates the viewer is a member of the target project, duplicates the upload reference into `project_attachments` or `project_issues + project_issue_attachments`, returns the new project/issue URL.

### UI wiring

- A single drag-state signal `$_chat_drop_active` is flipped true on `dragenter`-with-files and false on `dragleave` / `drop`. The full-cover overlay is bound `data-show="$_chat_drop_active"`.
- The composer's drop handler delegates to a new shared `web/static/chat-attach.js` helper that:
  - Sequentially uploads each file via XHR with progress.
  - Updates per-file row state via direct DOM manipulation (no Datastar morph per progress tick — too noisy).
  - On all-success, stashes the returned `upload_id`s into a `$attachment_ids` signal (JSON array).
- `PostSend` reads `attachment_ids[]` alongside `body`. Empty body + ≥1 attachment is valid (silent-file-share).
- Bubble rendering moves into a new `MessageAttachments(atts []AttachmentView)` templ that handles the grid + per-kind rendering.

### Service worker / video posters

- Video posters generated server-side on first upload via `ffprobe` when present in `$PATH`; cached alongside the upload as `<sha>_poster.jpg`. When ffprobe is missing, posters are skipped and the `<video>` falls back to the browser default (black frame until play).
- Audio waveform thumbnails are out-of-scope for v1.

## Verification

- Drop one image onto the presence panel → file uploads, bubble appears with the image inline.
- Drop three mixed files (image + pdf + mp4) onto `#messages` → single bubble with 3 previews / chips.
- Drop a 150 MB file → composer row shows a red "too large" error, no upload happens.
- Drop an .exe → row rejected with "file type blocked".
- Start a 50 MB video upload, hit ✕ cancel → row disappears, no chat row created.
- Send a chat with body + 2 attachments, lose network mid-upload → composer keeps the rows, displays "retry"; on retry click the upload resumes from zero (no chunk-resume in v1).
- As a member, open the message menu on a PDF chip → no "Extract" item.
- As a mod, open the same menu → "Extract to project" → "Save to Docs" → picks a project → the doc lands under that project's Docs tab with the original filename.
- As an admin, "New issue from this attachment" → issue opens with the file attached; chat bubble now shows "↗ in project X".
- Two viewers in different tabs each see the new bubble within the existing fat-morph window.
- `chat_message_attachments` survives a process restart; bubbles re-render identically.

## Friction

- **No chunked upload in v1.** A user on 4G dropping a 100 MB video has to babysit the tab. If the connection drops, retry restarts from zero. Spec accepts this; future revision adds tus.io-style resumable uploads.
- **MIME sniff vs. type spoof.** Content-sniff catches most cases but a `.docx` that's really a zip-bomb still uploads as zip. The denylist only blocks executable kinds; broader malware scanning is out-of-scope.
- **Bubble height for video / pdf.** A bubble with a 280-px-tall video + a 6-line body becomes visually heavy. Scroll-anchor still works because we keep the fat-morph + ExecuteScript scroll pattern.
- **Extract-to-issue defaults.** Subject prefilled from filename can produce ugly titles ("scan_2026-06-17_14-32-08.pdf"). The composer is editable, but expect users to leave the default. Acceptable for v1.
- **Original message stays intact when extracted.** No "move to project" mode that removes the bubble from chat — by design, since chat is the conversation thread and extracts are derivative.

## Interactions

- Depends on [[spec - forumchat - community web app with realtime chat and forum threads]] for the chat fan-out and message schema.
- Depends on [[spec - projects - per-community-collaborative-projects]] for the Docs tab + attachment schema this extract path reuses.
- Depends on [[spec - project-issues - per-project-issues-with-guest-share-links]] for the issue + issue-attachment schema the "New issue from this" path writes to.
- Affects [[spec - todos - personal-todos-from-chat-and-forum]] — a chat with attachments still surfaces the body as a todo source; attachments are not carried into the todo row.
- Affects [[claim - chat-uploads-allowlist-images-only]] — supersedes that restriction.

## Mapping

> [[internal/uploads/uploads.go]]
> [[internal/uploads/handler.go]]
> [[internal/chat/chat.go]]
> [[internal/chat/handler.go]]
> [[internal/projects/repo.go]]
> [[internal/projects/issues.go]]
> [[internal/storage/sqlite/migrations]]
> [[web/templ/chat.templ]]
> [[web/static/chat-attach.js]]
> [[web/static/paste.js]]
> [[web/static/app.css]]

## Future

- {[!]} Resumable / chunked upload (tus.io or custom) so 100 MB on flaky mobile doesn't restart from zero.
- {[!]} ffprobe video poster generation pipeline + fallback for environments without ffprobe.
- {[?]} Audio waveform thumbnails (server-side via ffmpeg) in place of the plain `<audio>` strip.
- {[?]} Drag-to-reorder attachments inside the composer before send.
- {[?]} Quota per community / per user — e.g. cap a community at 5 GB of chat uploads.
- {[?]} "Move to project" mode that removes the original chat bubble after extract — opt-in, behind a confirm dialog.
- {[?]} Inline OCR on extracted PDFs / images so the project Docs entry becomes searchable.
