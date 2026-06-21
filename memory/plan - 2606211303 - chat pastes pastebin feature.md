---
title: chat pastes — pastebin/hastebin inside a community
status: completed
created: 2026-06-21
---

# Plan — chat pastes (pastebin/hastebin)

## Context

Net-new feature, no existing spec. Members need to paste long code/markdown/text
without flooding a channel. Flow (from the request):

1. `/paste` slash command in chat (and a composer 📋 button) → creates a draft
   paste, redirects to `/c/{slug}/pastes/{pid}`.
2. That page shows a big textarea + format select + optional title.
3. On **Save** → renders + stores the paste, posts the paste URL into the
   source channel as the member, redirects back to that channel.

Reuses existing seams (no new patterns):

- Slash command shape: `chat.PostSend` + `isSlashCommand` (`internal/chat/handler.go:204,826`),
  bridged to other packages via closures wired in `main.go` (`Summary`/`Prompt`,
  `cmd/app/main.go:848,944`). New closure `chatHandler.NewPaste`.
- Post-to-channel + fan-out: model the agent share-to-channel closure
  (`cmd/app/main.go:470-496`) — `chatSvc.Send` → `chatBus`/`chatNewMsgBus`
  Broadcast → NATS publish → `RelayOut`.
- Markdown/code render: `render.RenderMarkdown` (`internal/render/markdown.go:117`).
  Code → fenced ```` ```lang ```` block; markdown → rendered directly. Reuses the
  existing goldmark+bluemonday pipeline; no new sanitizer.
- Handler shape: clone `bookmarks.Handler` (cid/cname/cslug/viewer helpers,
  `internal/bookmarks/handler.go:28-57`).
- templ leaf rule (§4.13): define `PasteView`/`PastePageData` in `web/templ`,
  map in the handler.
- Route mount: under `r.Route("/c/{slug}")` (`cmd/app/main.go:1209`), after the
  chat block.

## Decisions (sensible defaults, stated — no source conflict)

- **State model:** `posted_at` nil = draft. Editor renders when viewer == author
  AND draft; everyone else (and the author after posting) sees the read-only
  rendered view. Pastes are immutable after posting (pastebin semantics).
- **One control for format:** `language` field, default `go`. `markdown` →
  render as markdown; `text` → plain fenced; anything else → fenced code with
  that language token (CSS-styled `<pre><code class="language-xx">`, no
  server-side highlighting in MVP).
- **Chat message:** posted as the member (KindUser) so it reads as theirs. Body =
  optional bold title + the **relative** paste URL as an explicit
  `[url](url)` link (label==href survives `sanitizeUserMarkdown`,
  `internal/render/markdown.go:77`; relative avoids depending on `BASE_URL`).
- **Signals are page-local** via `data-signals` on the paste editor root —
  `paste_title`/`paste_language`/`paste_body` are NOT added to the global
  `InitialSignals` bag, because `paste_body` can be large and would otherwise
  ride every unrelated `@post`/`@get`. (Reasoned deviation from §4.2.)
- **channel_id `ON DELETE SET NULL`:** a paste outlives its channel; save/redirect
  falls back to `#general` if the channel is gone.
- **Draft orphans** (user navigates away without saving) are harmless (empty,
  unshared). No sweeper in MVP — noted as friction.

## Phases

### Phase 1 — schema + domain (no UI yet)
1. [ ] Migration `00047_pastes.sql` — `pastes` table (id, community_id,
   channel_id FK SET NULL, author_id, title, language, body, body_html,
   posted_at, created_at, updated_at) + community index.
2. [ ] `internal/pastes/pastes.go` — `Paste` struct, `Repo` (Create draft,
   ByID, Update-and-post), `Service` (CreateDraft, Save → render + mark posted).
   - => verify: `go build ./...`

### Phase 2 — handler + routes + wiring
3. [ ] `internal/pastes/handler.go` — `GetPage` (editor vs view), `PostNew`
   (create draft + redirect), `PostSave` (persist + post to chat + redirect).
   cid/cname/cslug/viewer helpers cloned from bookmarks.
4. [ ] Wire in `cmd/app/main.go`: build `pastes.Handler`, inject `PostToChat`
   closure (Send + fan-out), mount routes under `/c/{slug}`, set
   `chatHandler.NewPaste` closure.
5. [ ] `/paste` slash command in `chat.PostSend` + `NewPaste` field on
   `chat.Handler`.
   - => verify: `go build ./...`

### Phase 3 — UI (templ + CSS)
6. [ ] `web/templ/pastes.templ` — `PastePage` (editor + view branches),
   `PasteView`/`PastePageData` structs. `make gen`.
7. [ ] Composer 📋 button in `web/templ/chat.templ` `ChatComposer` →
   `@post('/c/{slug}/pastes/new?channel={channelSlug}')`.
8. [ ] CSS in `web/static/app.css` — `.paste-*` (editor card, full-height
   textarea, view container) using existing design tokens; mobile-friendly.
   - => verify: `make gen && go build ./cmd/app`

### Phase 4 — test + verify
9. [ ] `internal/pastes/service_test.go` — CreateDraft → Save round-trip
   (markdown + code), posted_at stamped, body_html non-empty. `t.TempDir()` DB.
10. [ ] `make test`; manual HTTP smoke (create → save → message in chat →
    redirect). Update README routes + CLAUDE.md if a new pattern lands.

## Verification

- `go test ./...` green; `make build` clean.
- `/paste` (and 📋 button) opens editor at `/c/{slug}/pastes/{pid}`.
- Save posts a clickable paste link into the source channel and redirects back.
- Paste link opens a read-only rendered view (code highlighted-class / markdown).

## Progress Log

- 2026-06-21 13:1x — Shipped all 4 phases on `task/chat-pastes`.
  - => migration `00047_pastes.sql`; `internal/pastes/{pastes.go,handler.go,service_test.go}`;
    `web/templ/pastes.templ`; CSS `.paste-*` in `app.css`; composer 📋 button.
  - => `chat.Handler.NewPaste` closure + `/paste` slash branch in `PostSend`.
  - => `main.go`: pastes handler + `PostToChat` closure (Send + Bus + NATS +
    relay, modelled on agent share-to-channel) + routes under `/c/{slug}`.
  - => render reuse: code → fence (fence-safe length) → `render.RenderMarkdown`;
    markdown rendered directly. No new sanitizer.
  - => chat message = `📋 **title**` + explicit `[url](url)` (label==href survives
    the no-hidden-URLs rewrite; relative, no BASE_URL dependency).
  - => signals page-local (`data-signals` on editor) — paste_body kept OUT of the
    global InitialSignals bag.
  - => verified: 5 service tests pass; full `go test ./...` + build clean; boot
    applies migration cleanly; HTTP smoke = register → /paste & 📋 → editor →
    save → link in #general + redirect → read-only `<pre class=language-go>` view.
  - friction: draft orphans (navigate away without saving) left in place — empty,
    unshared, harmless. No sweeper in MVP.
