---
status: active
created: 2026-07-02
---

# Plan — chat enterprise-grade design polish

## Context

The chat shell got its messenger pass on Jun 24 (`.chat-layout` full-bleed,
own/other bubbles, pill composer). A Playwright review (desktop 1440px +
mobile 390px, screenshots in session scratchpad) shows the remaining gaps to
"enterprise grade" (Slack/Linear-level):

- No **date separators** — every message repeats "15:04 Jun 15"; days blur
  together. Enterprise chat shows a divider per day + time-only stamps.
- **Authorless digest/system messages** render a red "?" identity dot
  (`mentionColor("")` → red, `mentionInitial("")` → "?") — reads as an error.
- No **scroll-to-latest** affordance — once scrolled up there's no way back
  except manual scrolling, and no "you're reading history" cue.
- No **empty state** for a fresh channel.
- **Mobile composer clip** (open rec from memory): the long placeholder wraps
  to two lines and clips inside the 1-row textarea.
- **Mobile meta wrap**: long display names wrap and push the timestamp to its
  own line — messy header inside bubbles.
- Roster/header polish: timestamps not tabular, roster rows lack hover state.

Related: `eidos/spec - forumchat - community web app with realtime chat and
forum threads.md`, CLAUDE.md §6 (fat-morph — MessagesContainer re-renders
fully on every event, so server-side day grouping is safe).

## Phases

### Phase 1 — Date separators + timestamps (status: open)

1. [ ] `web/templ/chat.templ` — render a `.day-sep` divider between message
   groups whose CreatedAt crosses a local-day boundary (pure server-side walk
   in `MessagesContainer`; label: Today / Yesterday / "Mon, Jan 15" (+year if
   not current)). Time stamps inside meta become time-only `15:04` with the
   full date in `title`.
   - => verify: `make gen && go build ./...`
2. [ ] CSS `.day-sep` (centered hairline + pill label) + `font-variant-numeric:
   tabular-nums` on meta timestamps, scoped under `.chat-layout`.
   - => verify: Playwright screenshot shows dividers.

### Phase 2 — Message list affordances (status: open)

3. [ ] Scroll-to-latest pill: `data-on:scroll__throttle.150ms` on `#messages`
   maintains FE-only `$_chat_at_bottom`; floating button (`data-show`) scrolls
   back down. Pure Datastar, no JS file.
4. [ ] Empty state: `len(messages)==0` renders `.chat-empty` (icon + "No
   messages yet" + hint) inside `#messages`.
5. [ ] Authorless identity dot: empty AuthorName/AuthorID → neutral muted
   "megaphone" dot instead of red "?" (guard in `bubbleColor` /
   `mentionInitial` call sites in `MessageView`).
   - => verify: build + screenshot of digest messages.

### Phase 3 — Header, roster, mobile (status: open)

6. [ ] Mobile composer: fix placeholder clip (shorter contextual placeholder
   "Message #channel" + CSS min-height/overflow guard).
7. [ ] Bubble meta: name gets `min-width:0` + ellipsis so the timestamp stays
   on the same line on narrow screens.
8. [ ] Roster polish: row hover surface, spacing rhythm; header/topic spacing
   tightened.
   - => verify: Playwright desktop + mobile after-shots, compare.

### Phase 4 — Verify + land (status: open)

9. [ ] `make test`, `go vet ./...`, full Playwright pass (desktop + mobile,
   empty channel + busy channel), before/after screenshots reviewed.
10. [ ] Commit per phase, merge `task/chat-enterprise-polish` → main ff-only,
    push.

## Progress Log

- 2026-07-02 15:05 — plan created from Playwright review of live UI.

## Adjustments

_(none yet)_
