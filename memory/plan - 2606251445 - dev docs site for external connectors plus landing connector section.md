---
name: plan-dev-docs-connectors
status: active
type: plan
tldr: Ship a self-contained `/dev/docs/*` developer documentation site (markdown in `./docs`, embedded via embed.FS, rendered as standalone HTML styled like policies/policygoogle.html) whose flagship doc is the external-connectors integration guide (auth model, JSON structs, curl + SDK examples), and add a comprehensive "Connectors / Developers" section to the SaaS landing page linking to it.
---

# Dev docs site for external connectors + landing connector section

## Context

- Spec: [[spec - connectors - external-chat-bots-as-human-members-over-signed-sse]]
  (`status: implemented`) — the integration surface the docs describe.
- SDK: `sdk-go/connector.go` (the Go client) + `examples/tinychat` (runnable
  demo) + `examples/README.md` (webhooks vs connectors prose) are the source
  material for the docs.
- Style reference: `policies/policygoogle.html` — a standalone, self-contained
  (no CDN deps) flat-light page (◇ mark, indigo `oklch(57% 0.19 272)` accent,
  serif display + sans prose + mono code, sticky bar, skip link, sticky TOC).
  Mirror its design tokens. (See palace: project_google_oauth_policy — sticky
  TOC dies if its grid parent has `align-items:start`.)
- Landing: `web/templ/landing.templ` `LandingPage()` composes sections
  (`landingHero`…`landingFinalCTA`); rendered by
  `internal/dashboard/dashboard.go:57` only when `webtempl.SaaSEnabled`.
- Markdown: `internal/render.RenderMarkdown` exists but runs bluemonday
  `UGCPolicy` which STRIPS heading `id`s (autoheading-id is added at
  `markdown.go:363` but sanitized away) → no TOC anchors. Dev docs are
  first-party TRUSTED content compiled into the binary, so they get their own
  goldmark renderer (GFM + autoheading-id, NO bluemonday) that keeps heading
  IDs for the TOC.
- Asset inlining: `web/templ/assets.go` `InlineStyle(name)` inlines
  `web/static/<name>` into the page head (used by the landing). Reuse for the
  dev-docs stylesheet.
- Route precedent: `policygoogle.html` is NOT wired into Go routes today
  (standalone file). The landing/export/invite public pages mount at router
  root outside auth. `/dev/docs/*` mounts the same way.

## Decisions

- **Embed location** — `docs/docs.go` (`package docs`) at repo root with
  `//go:embed *.md` + a `Manifest` slice (slug, title, summary, file). One
  manifest drives BOTH the index list and per-slug routing (unknown slug →
  404). Satisfies "create ./docs dir with markdowns, embed.FS them". `embed`
  forbids `..`, so the embed file lives beside the markdown, not in
  `internal/`.
- **Trusted renderer** — dedicated goldmark in `internal/devdocs/render.go`
  (GFM tables + fenced code + autoheading-id), no sanitization (first-party
  content), so `<h2 id>` survives. TOC extracted by regex over the rendered
  HTML headings.
- **Standalone page** — `web/templ/devdocs.templ` renders a full `<html>` doc
  (NOT `@Layout`, no datastar CDN, no app-shell scroll-lock) + inlined
  `web/static/devdocs.css` mirroring the policy tokens. Self-contained on our
  own SaaS domain.
- **Public, always mounted** — `/dev/docs` + `/dev/docs/{slug}` at router root,
  no auth (docs are public). Linked from the landing only (landing is
  SaaS-only), but reachable in self-host too.

## Phases

### Phase 1 — Docs content + embed package — status: open

1. [ ] `docs/docs.go` — `package docs`, `//go:embed *.md`, `embed.FS` +
   `Doc{Slug,Title,Summary,File,Order}` manifest + `Get(slug)` / `List()`.
2. [ ] `docs/connectors.md` — the flagship connector dev guide: what a
   connector is (vs webhooks/agents), the signed-URL/body-HMAC auth model,
   the `GET /bots/{id}/stream` wire (ready + message JSON structs), the
   `POST /bots/{id}/send` + moderation (`delete`/`ban`/`rename`) contract,
   **curl examples** for every endpoint, the Go SDK quickstart, reconnect +
   security notes, error table.
3. [ ] `docs/index.md` (overview / getting-started landing for the docs site)
   + `docs/webhooks.md` (sibling integration surface, brief) if cheap.

### Phase 2 — Trusted renderer + standalone page — status: open

4. [ ] `internal/devdocs/render.go` — goldmark (GFM + autoheading-id), render
   markdown→trusted HTML; `tableOfContents(html)` extracts h2/h3 → `[]TOCItem`.
5. [ ] `web/static/devdocs.css` — flat-light tokens mirrored from
   policygoogle.html; prose, code blocks, tables, sticky TOC (grid parent must
   NOT be `align-items:start`), responsive (TOC collapses under prose on
   mobile), focus states, `prefers-reduced-motion`.
6. [ ] `web/templ/devdocs.templ` — `DevDocPage(meta, tocHTML, bodyHTML)` full
   standalone doc + `DevDocsIndex(docs)` cards index; view-model structs local
   to `web/templ` (§4.13). `make gen`.

### Phase 3 — Handler + routes — status: open

7. [ ] `internal/devdocs/handler.go` — `GetIndex` (renders `DevDocsIndex`),
   `GetDoc` (manifest lookup → render → `DevDocPage`; unknown slug → 404).
8. [ ] Wire in `cmd/app/main.go`: `r.Get("/dev/docs", …)` +
   `r.Get("/dev/docs/{slug}", …)` at router root (public).

### Phase 4 — Landing connector section — status: open

9. [ ] `web/templ/landing.templ` — `landingConnectors()` section (what
   connectors unlock, the human-member model, a code/curl peek, CTA to
   `/dev/docs/connectors`); insert into `LandingPage()` flow; add a
   "Developers" nav link. `make gen`.

### Phase 5 — Verify + ship — status: open

10. [ ] `make gen && make build && make test`; boot with SAAS + CONNECTORS,
    Playwright screenshot `/` connector section + `/dev/docs` + `/dev/docs/connectors`
    (desktop + 375px). Codex read-only review of the new handler (untrusted URL
    slug input). Commit per phase, push, merge to main.

## Verification

- `/dev/docs` lists the docs; `/dev/docs/connectors` renders the guide with a
  working sticky TOC, syntax-styled code blocks, and tables.
- Unknown slug (`/dev/docs/nope`) → 404, no panic.
- Landing `/` shows the connectors section with a live link into the docs.
- Mobile 375px: no horizontal scroll, TOC reflows under prose.
- `go build` + `go test ./...` green; `templ generate` clean.
