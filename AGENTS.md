# AGENTS.md — forumchat

Onboarding for AI agents working on this codebase. Read this in full before
making changes. The lessons here cost real time; please don't re-discover them.

---

## 1. What this is

A small Go web app — a community space that combines a single realtime chat
channel with a forum. Single-binary, SQLite, NATS for pub/sub fan-out, datastar
for realtime UI, templ for HTML.

Full feature description: `README.md` and `eidos/spec - forumchat - community web app with realtime chat and forum threads.md`.
Implementation plan + progress log: `memory/plan - 2606131456 - implement forumchat MVP per spec.md`.

Status: **MVP complete and deployed**. All 10 phases of the plan are marked
`completed`. Tests pass; an end-to-end HTTP smoke is green. The chat UI uses
a "fat-morph" pattern (see §6).

Repo: `github.com/atvirokodosprendimai/forumchat`.

---

## 2. Quick orientation (read this first)

```
cmd/
  app/main.go     entry point — wires everything together
  cli/main.go     admin CLI (invite / role / ban / unban)
internal/
  config/         env-driven config (caarlos0/env + godotenv) + slog setup
  storage/sqlite/ DB open (modernc, WAL) + embedded goose migrations
  natsx/          NATS connect + subject helpers
  render/         markdown pipeline + datastar SSE helper (thin)
  httpx/          request logger + recover middleware
  auth/           users, sessions, register/verify/login, ban, profile
  community/      bootstrap single community + membership lookup
  chat/           chat domain + handlers (NATS + SSE + fat-morph)
  forum/          threads + posts + handlers (+ bridge to chat)
  presence/       in-process tracker + SSE handler
  uploads/        sha256 store + HMAC signed-URL handler
web/
  templ/*.templ   source templates — NEVER edit *_templ.go
  static/app.css  light theme
migrations/       UNUSED (real migrations live under internal/storage/sqlite/migrations)
```

Stack: Go 1.25+ / chi v5 / templ / **datastar v1** (Go SDK +
`github.com/starfederation/datastar-go/datastar`) / NATS core pub/sub /
modernc.org/sqlite / goose / goldmark+bluemonday / alexedwards/scs/v2 with
**memstore** / httprate / bcrypt.

---

## 3. Build, run, test

```sh
make tidy                  # go mod tidy
make gen                   # templ generate — runs *.templ → *_templ.go
make build                 # CGO_ENABLED=0 go build ./cmd/app
make run                   # gen + go run ./cmd/app   (env from .env if present)
make test                  # go test ./...
make up                    # docker compose up -d --build (app + nats + mailpit)
```

**You must `templ generate` after editing any `.templ` file.** The generated
`*_templ.go` files are committed but never hand-edited.

The full env-var reference is in `README.md`. Defaults Just Work in dev; the
two prod-critical secrets that boot rejects if left at dev values are
`SESSION_KEY` and `UPLOADS_SIGN_KEY`.

---

## 4. Datastar — read this whole section before touching any handler or templ file

We use Datastar v1 (CDN at
`https://cdn.jsdelivr.net/gh/starfederation/datastar@v1.0.2/bundles/datastar.js`).
**Check `https://data-star.dev/guide/getting_started` for the current latest version
before bumping.**

### 4.1 v1 attribute syntax — this is where most mistakes happen

| Use                     | Don't use                |
|-------------------------|--------------------------|
| `data-signals="{...}"`  | n/a (unchanged)          |
| `data-bind="email"`     | `data-bind-email`        |
| `data-on:click="…"`     | `data-on-click="…"`      |
| `data-on:keydown="…"`   | `data-on-keydown="…"`    |
| `data-init="…"`         | `data-on:load`, `data-on-load` *(load isn't a DOM event in v1)* |

The mount hook is `data-init` — fires once when the element enters the DOM
and re-fires every time it's morphed back in. We exploit this for the
scroll anchor (§6).

### 4.2 The signal bag

`web/templ/layout.templ` declares the global signal bag on `<body>` via the
`InitialSignals` const. Every input that needs to be read by a handler must
bind into one of those keys:

```templ
<input type="email" data-bind="email"/>
<input type="password" data-bind="password"/>
<button data-on:click="@post('/login')">Sign in</button>
```

When you add a new signal, add it to `InitialSignals` so the bag is
declared up-front. Datastar sends the **entire** bag on every `@post` /
`@get` (you can see them all in the request payload — `email`, `password`,
`subject`, `body`, `invite_code`, `display_name`, `avatar_url`,
`quoted_post_id`). Handlers read only the fields they need via a struct
with `json:"…"` tags.

### 4.3 SSE response pattern (`datastar-go` SDK)

Every action endpoint follows the same shape:

```go
func (h *Handler) PostSomething(w http.ResponseWriter, r *http.Request) {
    // 1. Read signals BEFORE NewSSE. NewSSE flushes the response body and
    //    the SDK panics if you try to read the body after.
    var in mySignals
    if err := datastar.ReadSignals(r, &in); err != nil { ... }

    // 2. Do the work (validate, persist, etc.)
    if err := doStuff(in); err != nil { /* render error fragment */ return }

    // 3. Open the SSE response.
    sse := datastar.NewSSE(w, r)

    // 4. Patch elements / signals; optionally Redirect.
    sse.PatchElementTempl(comp, datastar.WithSelector("#id"), datastar.WithModeOuter())
    sse.PatchSignals([]byte(`{"body":""}`))
    sse.Redirect("/somewhere")  // client-side navigation
}
```

**Never use `r.ParseForm()` in signal-driven handlers** — request bodies are
JSON, not form-urlencoded.

### 4.4 ⚠️ The scs session cookie + NewSSE bug — read in full

This bit me hard. If you mutate `scs` session state (login, logout, anything
calling `sm.Put` / `sm.Destroy`) inside a handler that uses `datastar.NewSSE`,
the cookie will **not** reach the client unless you commit it explicitly.

Why:

- `scs.SessionManager.LoadAndSave` wraps `w` in a `sessionResponseWriter`
  that writes `Set-Cookie` on the first `Write`/`WriteHeader` call. It also
  has an `Unwrap()` method.
- `datastar.NewSSE` calls `http.NewResponseController(w).Flush()`.
  `ResponseController` walks the writer chain via `Unwrap()` until it finds an
  `http.Flusher` and calls `Flush` on it — flushing the **underlying** writer
  directly, **bypassing** scs's wrapper. scs's `commitAndWriteSessionCookie`
  hook never fires, so `Set-Cookie` is never sent.

Fix — call `commitSession` (defined in `internal/auth/handlers.go`) **before**
`datastar.NewSSE`:

```go
PutLogin(r.Context(), h.Sessions, res.User.ID, res.Membership.CommunityID)
commitSession(h.Sessions, w, r)  // ← THIS
sse := datastar.NewSSE(w, r)
_ = sse.Redirect("/")
```

The required order for any future handler that touches session state:

```
ReadSignals → mutate session → commitSession → NewSSE → patches/Redirect
```

Regular HTML handlers (`GetVerify`, anything that calls `.Render(ctx, w)`) are
**not** affected — `Render` calls `Write` on the wrapped writer, which fires
scs's commit hook normally.

### 4.5 The chat fat-morph pattern (§6 below)

Chat updates are **never** "append a bubble." The whole `#messages` container is
fat-morphed on every event, plus a separate `#scroll-anchor` outer-morph that
re-triggers `data-init` and scrolls to the bottom. See §6.

### 4.6 Don't write bool signals from JS via hidden input

Hidden `<input data-bind="foo"/>` is the right pattern for **string** signals
that you want to set from JS (`paste.js`, `mention.js`, anything that needs to
mutate state outside a Datastar expression). The contract is:

```js
const host = document.querySelector('[data-bind="foo"]');
host.value = "hello";
host.dispatchEvent(new Event('input', { bubbles: true }));
```

This works because Datastar listens for `input`/`change` events on the bound
element and coerces the string value into the declared signal type.

**Bool signals don't survive this round-trip cleanly.** Setting `host.value =
"true"` vs `""` and relying on coercion is unreliable across Datastar
versions. So:

- **String signals from JS:** hidden input + dispatchEvent. Fine.
- **Bool / number signals from JS:** don't. Flip them inside a Datastar
  expression instead: `data-on:click="$foo=true"`, `data-on:blur="$bar=false"`,
  or wrap a JS call: `data-on:click="window.fcThing(); $foo=true"`.

We hit this with `mention_open`. Initial design used a hidden bool input; the
fix was to keep `mention_query` (string) on the hidden-input bridge but flip
`mention_open` in the Datastar expression itself (`$mention_open = true` after
the `fcMentionDetect` call returns).

### 4.7 Live morph via stable-id extract

Once a page has multiple pieces of state that all need to update after one
server action, **don't** try to PatchElementTempl every individual piece. Extract
all state-dependent UI into ONE templ component with a stable root id, and
patch that root after every transition.

Example from `web/templ/lobbies.templ`:

```templ
templ LobbyHostHeader(slug string, l LobbyView, guestURL string) {
    <div id="lobby-host-header">
        <h1>{ l.GuestName } · <span>{ l.Status }</span></h1>
        ...
        if l.Status == "open" {
            <button data-on:click="@post('/.../close')">Close</button>
        } else {
            <button data-on:click="@post('/.../reopen')">Reopen</button>
        }
        ...
    </div>
}
```

Every state-mutation endpoint (close, archive, reopen, promote, update-guest)
ends with:

```go
sse.PatchElementTempl(webtempl.LobbyHostHeader(slug, freshLobby, url))
```

Properties:

- **Same template covers all states.** Switch on `l.Status` inside; no per-state
  templates to keep in sync.
- **Patch is idempotent across pages.** If the host happens to be on the index
  list instead of the lobby page, the morph is a no-op (no `#lobby-host-header`
  in the DOM) — no branching needed in the handler.
- **Beats `sse.Redirect` to the same URL.** Redirect drops scroll position,
  closes any SSE streams, and flashes blank for ~100ms. Live morph keeps
  everything else intact.
- **Beats per-button targeted patches.** Multiple sibling patches (status badge
  + button row + composer visibility) multiply the patch calls without buying
  anything; one root patch is cleaner.

Apply the same idea to:
- A composer that needs to hide when state changes (`if l.Status == "open"
  { @LobbyComposer(...) }` inside the same wrapper).
- Edit dialogs where saving needs to refresh both the row and the form.

### 4.8 Debounced input + composer signal-naming

For typeahead / live-search / autocomplete, use `__debounce.Nms` on the input
event. We use 150ms for chat @mentions:

```templ
<textarea
  data-bind="body"
  data-on:input__debounce.150ms={ "el.style.height='auto'; el.style.height=Math.min(el.scrollHeight,200)+'px'; if(window.fcMentionDetect(el)){$mention_open=true; @get('/c/" + slug + "/chat/mention')} else {$mention_open=false}" }
></textarea>
```

Two attributes (`data-on:input` + `data-on:input__debounce.150ms`) on the same
element is **not** guaranteed to register two listeners — collapse to ONE
attribute with all the work inside, debounced together. Resize that's delayed
by 150ms is imperceptible; a server call per keystroke is not.

**Signal-naming convention** — when a new feature reuses the composer shape
(paste/drop/textarea), give the signals a feature-specific prefix so they don't
collide with chat's `body` / `image_data`. The lobbies composer uses
`lobby_body` / `lobby_image_data`; clearing them via `PatchSignals` after send
is `{"lobby_body":"","lobby_image_data":""}`. JS helpers (`fcDropImage`,
`fcPasteImage`) take the signal name as a string parameter — they work for any
feature.

### 4.9 Server-driven step transitions (multi-step forms)

For multi-step UI where the **server** owns "which step the user is on" (login
step 1 → step 2, lobby invite wizard, etc.), wrap the step shell in one stable
id and render a per-step templ inside:

```templ
templ LoginPage() {
    @Layout("Sign in", Viewer{}) {
        <div id="login-card">
            @LoginStep1()
        </div>
    }
}

templ LoginStep1() {
    <section id="login-card" class="card narrow">
        <input data-bind="email" ... />
        <button data-on:click="@post('/login/check')">Continue</button>
    </section>
}

templ LoginStep2(email string) {
    <section id="login-card" class="card narrow">
        ...password input...
        <button data-on:click="@post('/login')">Sign in</button>
        <button data-on:click="@post('/login/magic')">Email me a link</button>
        <button data-on:click="@post('/login/back')">use a different email</button>
    </section>
}
```

Both step templates render the **same root id** — `PatchElementTempl` swaps
them. Signals declared in `InitialSignals` carry between steps, so step-1's
email is still in the bag when step 2 renders.

Server-driven steps beat client-driven (`data-show="$step===1"`) when:
- Step transitions depend on a DB lookup (login: does this email exist?
  doesn't matter, we don't reveal it anyway).
- You want the URL/back-button to behave like the server is in charge.
- You don't want to ship the step-2 markup until step 1 has been submitted.

### 4.10 Anti-enumeration in signal-driven handlers

Whenever a handler's response shape could reveal "does this row exist", make
the shape **identical** across hit / miss. Two patterns:

1. **PostLoginCheck never queries the DB.** Every non-empty email gets the
   same `LoginStep2` fragment. The DB check happens at the next step
   (`PostLogin` errors with the generic "invalid email or password" message,
   identical for "no such user" and "wrong password").

2. **PostLoginMagic always renders "check your email".** `Service.IssueMagicLink`
   returns nil silently when the email is unknown, AND when the mailer fails.
   The handler doesn't branch on the result — always renders the same
   terminal page.

The wording matters too. "If `<email>` is a registered address, a one-time
link is on its way" confirms NOTHING and reads natural.

### 4.11 Per-lobby / per-X Bus over per-community Bus

`chat.Bus` is community-wide: every Send call wakes every open chat SSE in the
process. Fine for chat because every member sees the same channel.

`lobbies.Bus` is **per-lobby** — Subscribe takes a `lobbyID` and only that
lobby's broadcast wakes its subscribers. The map is `map[string]map[chan]`.
First sub on a new id creates the inner set; last unsub deletes it. Idle
lobbies have no entry, no memory cost.

Use per-X Bus whenever:
- A single page only cares about one row's events (one lobby, one project,
  one room).
- Many rows can exist in parallel (don't fan out to all of them on every
  write).

Stick to a single shared Bus when:
- Every viewer is on the same surface and broadcast-to-all is the actual
  semantic (chat, presence).

NATS subjects follow the same shape:
- `community.<cid>.chat` — community-wide
- `community.<cid>.lobby.<lid>` — per-lobby
- `community.<cid>.forum.thread.<tid>` — per-thread

### 4.12 EDA — Datastar is a DOM event bus, so use it like one

Datastar's `data-on:*` accepts **any** DOM event name, not just `click`/`input`.
`evt` is the actual `Event` object in scope; `evt.detail` carries the
`CustomEvent` payload. So the whole event-bus / pub-sub pattern from front-end
EDA is built in — you just don't always see it because we tend to reach for
per-element `data-on:click` first.

When you have **N similar triggers that all do the same thing**, replace N
per-element handlers with: one `dispatchEvent` per trigger + one
`data-on:<event-name>` listener at a common ancestor. Bubbling does the
routing.

#### The pattern

```templ
<!-- producers: many of these -->
<button data-on:click="el.dispatchEvent(new CustomEvent('fc:open-todo',{bubbles:true,detail:{id: 'msg-123', title: 'Pay invoice'}}))">
    ☑ To-do
</button>
<button data-on:click="el.dispatchEvent(new CustomEvent('fc:open-todo',{bubbles:true,detail:{id: 'msg-456', title: 'Reply to client'}}))">
    ☑ To-do
</button>

<!-- consumer: one of these, somewhere up the tree -->
<div data-on:fc:open-todo="$todo_open_source = evt.detail.id; $todo_title = evt.detail.title">
    @TodoDialog(slug)
</div>
```

Now each button knows nothing about which dialog is mounted, the dialog knows
nothing about how many buttons fire, and adding a new trigger is one line of
templ.

#### Modifiers stack on custom events too

```templ
<div data-on:fc:bookmark__debounce.300ms="@post('/bookmark')"></div>
<div data-on:fc:scroll-bottom__window="document.querySelector('#messages')?.scrollTo({top:1e9})"></div>
```

`__window` is the big unlock — register a listener globally without caring
where in the DOM the producer lives. Outside-click handlers, Esc handlers,
"close all open menus" handlers all live as one `data-on:click__window` /
`data-on:keydown__window` at the layout level.

#### Where this would have saved code (apply to next refactor)

- **`web/static/paste.js`** still has plain
  `document.addEventListener('click', ...)` + `document.addEventListener('keydown', ...)`
  for the close-on-outside-click affordance on `<details class="msg-menu">`.
  Equivalent Datastar in `layout.templ`:
  `data-on:click__window="document.querySelectorAll('details.msg-menu[open]').forEach(d => d.contains(evt.target) || (d.open=false))"`.
  No JS file needed.
- **`web/templ/chat.templ` per-message menu** has N copies of the same
  `data-on:click="$bm_open_msg = '<id>'"` etc. Could be:
  trigger: `data-on:click="el.dispatchEvent(new CustomEvent('fc:bookmark',{bubbles:true,detail:{id: '...'}}))"`,
  consumer once: `data-on:fc:bookmark="$bm_open_msg = evt.detail.id"`.
  Same noise per-trigger today; the win is when the consumer logic grows.

#### Signal patches are events too

`data-on-signal-patch` fires whenever ANY signal is patched (server or client):

```templ
<div data-on-signal-patch="console.log('signals changed', patch)"></div>
<div data-on-signal-patch-filter="{include: /^todo_/}" data-on-signal-patch="@get('/todos/preview')"></div>
```

Use for: cross-cutting state observers (analytics, autosave drafts, dirty-flag
UI). Don't use for normal "I changed signal X so re-fetch" — that's what
`data-computed` and `data-effect` are for.

#### When to NOT use custom events

- One trigger, one handler, no other listeners — inline `data-on:click="..."`
  is shorter.
- The handler needs the producer's surrounding template scope (loop index,
  parent struct field) that doesn't fit cleanly in `detail`. Then a closure
  via the inline expression is fine.
- Cross-tab coordination — that's NATS, not DOM events.

#### Rule of thumb

When two buttons in the same templ would dispatch the **same** `@post` or set
the **same** signal, that's the moment to refactor: hoist the listener to the
nearest common ancestor, emit a `bubbles:true` custom event from each button.

### 4.13 templ ↔ domain import cycle

`web/templ` is a leaf package — it **must not** import any `internal/<domain>`
package. Domain handlers import `web/templ`, never the other way around.

We hit a cycle when `chat.templ` imported `internal/chat.Message`. Fix:
`web/templ` defines its own view-model structs (`MsgView`, `ThreadView`,
`PostView`, `ChatPageData`, etc.) and each handler maps `domain → view` via
small adapter funcs (e.g. `toMsgView`).

When you add a new domain, follow the same pattern: define a local
`SomethingView` struct in `web/templ`, map in the handler.

---

## 5. The `Viewer` struct & layout

Every page-rendering handler must pass a `webtempl.Viewer` to `Layout`. The
layout decides the topbar nav (Sign in / Register vs Chat / Forum / Profile /
Sign out) from it:

```go
type Viewer struct {
    IsAuthed      bool
    DisplayName   string
    Role          string   // member|moderator|admin
    CommunityName string
}
```

Each handler package defines its own tiny `viewer(r)` helper that builds this
from `auth.FromContext(r.Context())` + the bootstrap community name.

When you add a new page handler, **always** thread a `Viewer` through to
`Layout` — there's no global state on the client to fall back on.

---

## 5b. Membership approval queue (Discord-style)

`memberships.approved_at` (added in migration 00002) gates access. Verified
users get a row with `approved_at = NULL` and are bounced to `/pending` by
the `auth.RequireApproved` middleware until an admin clicks Approve in
`/admin`. Admins bypass the check (so they can reach `/admin` to approve
the queue in the first place).

Bootstrap rules:
- The CLI `forumchat-cli role <email> admin` makes a user an admin
  regardless of `approved_at`. Use it once to seed the first admin.
- The admin can then self-approve via the UI.

Invite codes (also migration 00002) gained `max_uses` and `uses_count`:
- `max_uses = NULL` → unlimited reuses (Discord-style).
- `max_uses = N`   → rejected after N consumers (still useful for one-off
  invites).
- `uses_count` increments on every consume; `used_by` / `used_at` stamp the
  first consumer for legacy lookup.

Use `auth.Service.IssueInvite(ctx, communityID, createdBy, maxUses)` —
`maxUses` is `*int`, pass `nil` for unlimited.

## 5c. Ban + content cleanup

`POST /admin/ban?id=<membership_id>` reads four boolean signals
(`cleanup_chat`, `cleanup_threads`, `cleanup_posts`, `ban_hours`) and:

1. Stamps `memberships.banned_until` with `now + ban_hours` (or year 9999 if
   `ban_hours = 0`).
2. Calls `auth.Repo.CleanupUserContent(userID, communityID, opts)` which
   soft-deletes the banned user's chat messages / threads / posts per the
   options.
3. If any chat was wiped, pings `chat.Bus.Broadcast()` so every open chat
   tab fat-morphs.

The CLI mirrors this: `forumchat-cli ban <email> [duration] [cleanup]`
where `cleanup` is `chat,threads,posts` or `all`.

## 6. Chat — the fat-morph pattern

The chat UI is the most subtle piece. Read this before editing
`internal/chat/handler.go` or `web/templ/chat.templ`.

### 6.1 What the FE sees

`#messages` is a fixed-height (`overflow-y: auto`) bubble container. It carries
a `data-init` that scrolls itself to its own bottom on the FIRST mount (initial
page load).

### 6.2 What happens on send (or any chat event)

```
PostSend → persist → load latest 100 → emit:

  event: datastar-patch-elements              ← #messages (outer-morph) full latest-100
  event: datastar-execute-script              ← scroll #messages to its own bottom
  event: datastar-patch-signals               ← {"body":""}  clears composer
```

### 6.3 Why `ExecuteScript` and not a "scroll-anchor div with data-init"

We tried the anchor-div approach and it does NOT work:

- `#messages` has its own `overflow-y: auto`. The scrollable region is
  `#messages` itself, not the page. A sibling `<div>` calling
  `el.scrollIntoView()` scrolls the document — the chat stays put.
- Even if the anchor were INSIDE `#messages`, outer-morphing an element with
  the same id does NOT re-mount it. idiomorph treats it as the same element
  and patches in place. `data-init` only fires on first mount, so the second
  patch is a no-op.

`sse.ExecuteScript(\`document.querySelector('#messages')?.scrollTo({top: 1e9, behavior: 'smooth'})\`)`
is the unambiguous and reliable form. It runs every time the patch lands and
addresses the scroll container directly. For the INITIAL page load (no SSE,
just rendered HTML), the `data-init` on the messages container itself handles
the first scroll.

### 6.3a In-process Bus + NATS

A subtle bug: when NATS is unreachable, `Publish` silently no-ops, so
multi-tab updates within the same process never fire. Fix: every chat
write path calls **both**:

- `chat.Bus.Broadcast()`  — fans out to every open SSE stream in *this*
  process via an in-memory channel map.
- `nats.Conn.Publish(subject, []byte("changed"))` — fans out across
  processes when NATS is up.

`chat.Handler.GetStream` subscribes to **both** the local Bus and the NATS
channel and calls `loadRecent` + `fatMorph` on any signal from either.
Same-process realtime works without NATS; cross-process realtime works
when NATS is up.

This is also why `chat.Handler.PostDelete` works for non-mod viewers in
the same process — the mod's delete triggers the Bus, every chat SSE
stream refetches, and the non-mod sees the `[message removed]` placeholder
that `MessageView` renders when `m.Deleted && !isMod`.

### 6.3b Chat replies + promote-to-thread

`chat_messages.reply_to_id` (migration 00002) lets a message reference an
earlier message in the same channel. The signal `$reply_to_id` is set by
the per-bubble `reply` button (`data-on:click="$reply_to_id = '<msg-id>'"`)
and read by `PostSend` from the request body. The composer shows a small
"Replying — …id" hint via `data-show="$reply_to_id !== ''"` and a cancel
button that clears the signal back to `''`. The `Send` handler clears
both `body` and `reply_to_id` via `PatchSignals`.

Rendered replies show a small `<blockquote>` snippet (≤ 80 chars of the
parent's body_md, eagerly JOINed in `Repo.listBefore`).

Chat → forum promotion: each non-system bubble carries a `→ thread`
button visible when the viewer is the author OR mod/admin. It posts to
`/forum/promote-chat?id=<msg-id>`, which loads the message via
`chat.Repo.ByID`, builds a thread (subject = first line, body = full
markdown), and rides the normal forum→chat bridge to publish a
`thread_announce` back into the channel. `chat.Bus.Broadcast()` plus the
NATS publish ensure open tabs refresh.

### 6.4 NATS as a signal, not a payload

We do **not** publish rendered HTML over NATS anymore. Publishers send a
single byte string `"changed"` to `community.<id>.chat`. Each subscriber
SSE handler refetches the latest 100 from SQLite and emits its own
`fatMorph`. This decouples wire format from rendering and means the
publisher doesn't need to know per-viewer state (e.g. whether the viewer is
a mod, which changes the bubble HTML).

Same pattern applies to the forum → chat bridge: thread insert writes the
`thread_announce` `chat_messages` row, then pings the same NATS subject.

### 6.5 What this means in code

```go
const RecentLimit = 100   // FE never holds more than this in the DOM

func fatMorph(sse *datastar.ServerSentEventGenerator, views []webtempl.MsgView, isMod bool) error {
    if err := sse.PatchElementTempl(
        webtempl.MessagesContainer(views, isMod),
        datastar.WithModeOuter(),
    ); err != nil { return err }
    return sse.ExecuteScript(
        `document.querySelector('#messages')?.scrollTo({top: 1e9, behavior: 'smooth'})`,
    )
}
```

### 6.6 Removed flows

- `GetOlder` / `/chat/older` — gone. The FE intentionally doesn't keep older messages.
- Per-bubble NATS publish payloads — gone.
- `renderSystemFragment` in `internal/forum/handler.go` — gone.

Don't bring them back without rethinking the whole pattern.

---

## 6b. CQRS in this codebase — what writes and reads actually look like

We don't have separate `commands/` and `queries/` directories like a textbook
CQRS layout. The same shape shows up in a less formal version that's worth
naming so future you doesn't reinvent it.

### Shape per feature package (`chat`, `forum`, `lobbies`, `projects`, …)

| File | Role |
|---|---|
| `repo.go` | All SQL. Read methods (`Recent`, `ByID`, `ListByCommunity`, `SearchMembersByDisplayName`, `RecentMessages`) AND write methods (`Create`, `AppendMessage`, `UpdateStatus`). Repo is stateless; everything is via `*sql.DB`. |
| `service.go` | Write-side orchestration. Validates input, renders markdown via `internal/render`, calls Repo write methods, returns the persisted thing. **Single writer per concept.** `chat.Service.Send`, `lobbies.Service.Mint`, `auth.Service.IssueMagicLink`. |
| `handler.go` | HTTP boundary. Reads signals, calls Service for writes, calls Repo directly for reads, patches SSE. |
| `bus.go` *(optional)* | In-process per-X fan-out for SSE streams. See §4.11. |

### The "command" side — single writer

Every state-mutating path goes through one `Service.<Verb>` that:

1. Validates input (returns typed sentinel errors: `ErrEmptyBody`,
   `ErrClosedOrExpired`, `ErrPromoteNeedsEmail`).
2. Renders any user-supplied markdown into stored HTML at write time
   (`render.RenderMarkdown` is the gateway; pre-rendering on write keeps the
   read path cheap and bluemonday's sanitizer doesn't run on every render).
3. Calls Repo write methods inside a transaction when multiple rows are
   touched together (see `auth.Service.Register` for the canonical example
   with users + verification_tokens + invite consume in one tx).
4. Returns the persisted row.
5. **Does not** broadcast — that's the handler's job. Keeps the service
   testable without a Bus/NATS mock.

### The "query" side — many readers

Reads go straight from handler → Repo, with no Service hop unless there's a
viewer-aware step (e.g. promote-to-member needs auth.Service):

```go
// chat/handler.go
func (h *Handler) GetPage(...) {
    views, _ := h.loadRecent(r.Context())  // → Repo.Recent
    _ = webtempl.ChatPage(...).Render(r.Context(), w)
}
```

The same handler can run many concurrent reads (one per open SSE stream, one
per page load) and they never block writes. SQLite's WAL mode (default) gives
us reader/writer concurrency, so this works fine at the scale we target.

### The "read model is a reusable pure function" mental model

The cleanest way to think about the write↔read split here is **not** "writes
push HTML to readers". It's:

> The write side mutates the DB and publishes an event saying *which id
> changed*. The read model is a pure function `(id) → struct → templ` that's
> called from two unrelated entry points: initial page load and the SSE event
> loop. Neither entry point knows or cares about the other.

This matters because:

- **The read model can ship without the write model.** A new viewer page,
  reporting query, exported PDF — all reuse the same `(id) → struct` step.
- **The write model can be replaced** (raw SQL → service layer → outbox →
  whatever) without touching reader code.
- **The wire payload is minimal.** Just the id (or the small set of ids) that
  changed. The reader is the source of truth for "what HTML should this id
  look like *right now*", queried fresh on every event.

Concretely:

```go
// ---------- the read model: one pure function, used everywhere ----------
// internal/lobbies/repo.go
func (r *Repo) RecentMessages(ctx context.Context, lobbyID string, limit int) ([]LobbyMessage, error)

// internal/lobbies/handler.go
func renderMessages(ctx context.Context, repo *Repo, lobbyID string) (templ.Component, error) {
    msgs, err := repo.RecentMessages(ctx, lobbyID, RecentLimit)
    if err != nil { return nil, err }
    return webtempl.LobbyMessages(messagesToView(msgs)), nil
}

// ---------- entry point 1: page load ----------
func (h *Handler) GetHostView(w http.ResponseWriter, r *http.Request) {
    comp, _ := renderMessages(r.Context(), h.Repo, lobbyID)
    _ = webtempl.LobbyHostView(LobbyHostViewData{ /* ... */ Messages: ... }).Render(r.Context(), w)
}

// ---------- entry point 2: SSE event loop ----------
func (h *Handler) GetHostStream(w http.ResponseWriter, r *http.Request) {
    sse := datastar.NewSSE(w, r)
    local, unsubscribe := h.Bus.Subscribe(lobbyID)
    defer unsubscribe()
    natsCh, _ := subscribeNATS(h.NATS, natsx.LobbySubject(cid, lobbyID))

    // initial sync — call the SAME read model
    if comp, err := renderMessages(r.Context(), h.Repo, lobbyID); err == nil {
        _ = sse.PatchElementTempl(comp)
    }
    for {
        select {
        case <-r.Context().Done(): return
        case <-local:  // in-process Bus
        case <-natsCh: // remote NATS — payload carries the id but we don't even need it here
        }
        if comp, err := renderMessages(r.Context(), h.Repo, lobbyID); err == nil {
            _ = sse.PatchElementTempl(comp)
        }
    }
}
```

### The write side — emit the id, nothing else

The write handler ONLY: validates → persists → publishes a tiny "X changed"
event. It does NOT compose HTML, does NOT know which clients are watching,
does NOT call the read model.

```go
func (h *Handler) PostHostSend(w http.ResponseWriter, r *http.Request) {
    // 1. validate + persist (write model, single writer)
    msg, err := h.Svc.Send(ctx, SendInput{LobbyID: lobbyID, ...})
    if err != nil { return }

    // 2. echo the new state back to the actor that just posted, then exit
    //    (their SSE stream will also receive the broadcast — this PatchElementTempl
    //    is purely UX latency hiding, optional)
    if comp, err := renderMessages(ctx, h.Repo, lobbyID); err == nil {
        _ = sse.PatchElementTempl(comp)
    }
    _ = sse.PatchSignals([]byte(`{"lobby_body":"","lobby_image_data":""}`))

    // 3. publish the id that changed — every other open viewer's SSE loop
    //    will re-render via the read model.
    h.broadcast(r.Context(), lobbyID)
}
```

The broadcast helper does double duty (in-process + cross-process):

```go
func (h *Handler) broadcast(ctx context.Context, lobbyID string) {
    if h.Bus != nil {
        h.Bus.Broadcast(lobbyID)  // payload = the id itself, via map key
    }
    if h.NATS != nil && h.NATS.IsConnected() {
        _ = h.NATS.Publish(natsx.LobbySubject(h.cid(ctx), lobbyID), []byte(lobbyID))
    }
}
```

### Wire-payload guidance

| Scenario | Payload |
|---|---|
| Stream subscribes to one specific row's subject (`community.<cid>.lobby.<lid>`) | The id can be empty / `"changed"` — the subject IS the id. We do this for lobbies / per-thread forum. |
| Stream subscribes to a broad subject (`community.<cid>.chat`) and many rows can change | Payload = the changed id, so the consumer can decide whether it cares (skip if the id isn't visible in its current viewport). |
| Multiple ids changed atomically | Payload = JSON array of ids, OR publish multiple events. Prefer one-event-per-id; keeps consumers stateless. |

We deliberately do **not** put domain data on the wire (rendered HTML,
serialised structs, anything that would let a consumer skip the DB read).
Reasons:

- The DB is the source of truth at the instant the consumer renders. Wire-state
  goes stale between "write committed" and "consumer drains its queue".
- Permissions check at render time (mods see deleted messages, regular users
  don't). A pre-rendered payload would have to be re-rendered per-viewer
  anyway.
- The wire stays cheap — "changed" or a uuid fits in a packet, no GC pressure.

This is the "fat-morph" pattern §6 calls out for chat. Same shape applies to
lobbies, forum, project discussions. Don't try to be clever and send only the
diff — re-rendering the whole list with templ is cheap enough and morph keeps
the DOM in sync.

### Subscribe-once handler — browser → handler → NATS

The browser doesn't talk to NATS. It opens one long-lived SSE connection to
the handler, and the handler is the one subscribed to NATS (and the
in-process Bus). This is the shape every realtime page in forumchat follows:

```
browser
  └── EventSource('/c/<slug>/lobbies/<id>/stream')   ← Datastar opens this via data-init="@get(...)"
        └── handler.GetHostStream
              ├── Bus.Subscribe(lobbyID)              ← local fan-in
              └── NATS.ChanSubscribe("community.<cid>.lobby.<lid>")  ← remote fan-in
                    ↑
        every write handler calls h.broadcast(ctx, lobbyID), which publishes here
```

Net effect: write traffic on any node lights up every subscriber on every
node — without the read model and write model knowing about each other.
Replace NATS with anything else (Redis pub/sub, a message bus, a poll loop) by
swapping one package; nothing else moves.

### When to add a Service vs put logic in Repo

- **Repo**: just SQL. No business rules, no rendering, no IDs minted (caller
  supplies). One file per feature.
- **Service**: needs more than one Repo call, or owns rendering, or mints
  IDs/tokens, or has a state-machine validation. Write the simplest version
  first and graduate Repo → Service when a second write path needs the same
  validation.

`internal/admin/admin.go` is the boundary case — admin handlers call
`auth.Repo` directly because the operations are one-shot SQL (approve / ban /
remove). When the third one needed a guard (`CountAdmins` for last-admin
removal), the guard went into the handler, not into a new `admin.Service`.
That's fine — feature surface stays light. Promote it later if it grows.

### Anti-patterns

- **No "smart" Repo**: don't put markdown rendering, validation, or push
  notification inside repo methods. They run inside transactions and you'll
  end up holding the DB lock across a network call.
- **No silent writes from queries**: a `GetThing` handler must not write
  (don't auto-bump activity timestamps from a GET). Side-effects belong on
  POST/PUT/DELETE so caching, CSRF, and rate-limiting work as expected.
- **No service-to-service direct imports**: if `lobbies.Service` needs
  `auth.Service.IssueInvite`, declare a local `InviteIssuer` interface in
  `lobbies/service.go` and depend on the interface. Wire concrete types in
  `cmd/app/main.go`. See `lobbies/service.go` for the canonical pattern.

---

## 7. NATS subjects

```
community.<id>.chat            chat fan-out  (payload: "changed", subscribers refetch)
community.<id>.forum           forum events  (reserved; not actively used by chat fat-morph)
community.<id>.forum.thread.<tid>  per-thread fan-out (forum thread page)
community.<id>.presence        presence updates (reserved; presence currently uses in-process Tracker)
community.<id>.lobby.<lid>     per-lobby fan-out (guest access, see §4.11)
```

Connection is best-effort: if NATS is unreachable the app boots fine, chat
works locally for the sender only, and presence falls back to whatever this
single process knows. **Don't add code that errors out on NATS being down.**

---

## 8. SQLite (modernc) — the FK ordering trap

`modernc.org/sqlite` is opened with `foreign_keys=ON`. Some FK constraints
imply a specific insert order across rows; check the schema before writing
multi-row transactions.

Example I hit: `invite_codes.used_by` FK references `users(id)`. The original
register transaction consumed the invite first, then inserted the user. The
invite-consume `UPDATE` set `used_by=newUserID` to a row that didn't exist
yet → FK failure (787). Reorder: insert user → consume invite.

Single-writer pattern: we set `MaxOpenConns=1` because WAL + modernc means
one writer at a time. Don't bump this without understanding WAL contention.

---

## 9. scs sessions — in summary

- `scs/v2` with a **SQLite-backed store** (`internal/auth/sqlstore.go`) so
  sessions survive process restart. `scs/sqlite3store` is incompatible
  because it depends on CGO `mattn/go-sqlite3`; ours uses the project's
  modernc handle directly.
- `NewSQLStore` self-heals: it runs `CREATE TABLE IF NOT EXISTS sessions`
  in its constructor so the store works even when migration 00002 hasn't
  been applied yet. Migration 00002 still creates the table + index, the
  self-heal is the belt-and-braces.
- Cookie name: `forumchat_session`. HttpOnly, SameSite=Lax. `Secure` flag
  set automatically when `ENV=prod`.
- Identity is loaded in `auth.Loader` middleware, surfaced via
  `auth.FromContext(ctx) → (Identity, ok)`.
- Bans destroy the active session at next request (`Loader` checks
  `Membership.IsBanned(now)`).

---

## 10. Conventions and Effective Go

We follow the project-wide skill rules (chi v5, templ, goose, NATS, DataStar
Go SDK, DDD-ish layout) with these deliberate deviations / nuances:

- **No gorm.** Plain `database/sql` with hand-written queries against modernc.
  Each domain has its own `Repo` struct (`internal/auth/repo.go`,
  `internal/forum/forum.go`, etc.). Reason: small surface, easy to reason
  about, no abstraction tax. If you add gorm, you change the project shape.
- **No urfave/cli yet.** `cmd/cli/main.go` parses `os.Args` directly. Fine
  for the 4 subcommands today; if commands grow past ~6, refactor to urfave.
- **No gomarkdown.** We use `yuin/goldmark` + `microcosm-cc/bluemonday`. Don't
  swap unless you have a reason — goldmark has GFM extensions and our
  bluemonday policy is dialled in.
- **No ollama integration.** Reserve `github.com/eslider/go-ollama` for
  features that haven't been requested yet.
- **CQRS** in the loose sense: single SQLite writer, many readers via the
  SSE streams. Not a formal CQRS pipeline; treat the SSE fan-out as the
  "read model" side.

Standard Effective Go applies otherwise: `gofmt`, MixedCaps, return errors
(never panic — except where datastar SDK itself panics on flush failure,
which is by design), small interfaces, document exported symbols starting
with the symbol name.

---

## 11. Testing

`go test ./...` runs everything. Coverage today:

- `internal/auth/password_test.go` — bcrypt round-trip, short-password rejection.
- `internal/auth/service_test.go` — full register → verify → login flow with
  invite invalid / reuse / unverified / bad-password edge cases. SQLite
  tmpdir per test (`t.TempDir()`).
- `internal/uploads/uploads_test.go` — save+sign+verify round-trip, bad MIME,
  oversize. **Note**: when adding an upload test, you must first insert a
  `users` row to satisfy the `owner_id` FK (see existing setup helper).

When you add a new domain handler, write at minimum a happy-path service
test that uses `sqlite.Open` + `sqlite.Migrate` against a `t.TempDir()` DB.
Don't reach for httptest for everything — the service layer is where the
interesting logic lives.

---

## 12. Branch & commit workflow

This repo has a pre-tool hook that **blocks edits on main**. Always:

```sh
git checkout -b task/<short-description>
# edit, build, test
git add -A
git commit -m "..."
git checkout main
git merge --ff-only task/<short-description>
git push origin main
```

A `claude-mem` plugin auto-appends to `CHANGELOG.md` after every commit. To
avoid merge thrash, `git checkout -- CHANGELOG.md` before `git checkout main`
when the change is irrelevant.

Commit messages: keep `feat(scope):`, `fix(scope):`, `chore(scope):`,
`docs(scope):` style. Co-author line for AI-assisted commits if applicable.

---

## 13. Common errors I made — don't repeat them

| Error                                                          | What actually happened                                                                       | Fix                                                                                                             |
|----------------------------------------------------------------|----------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------|
| `data-on-click=`, `data-on-load=`, `data-bind-foo`             | Looks fine, datastar quietly ignores them                                                    | v1 syntax: `data-on:click`, `data-init`, `data-bind="foo"`                                                       |
| `r.ParseForm()` in a `@post` handler                           | Body is JSON, not form-urlencoded                                                            | `datastar.ReadSignals(r, &struct{…}{})` BEFORE `NewSSE`                                                          |
| Login "works" but next request shows logged out                | scs's `Set-Cookie` hook bypassed by datastar's `Flush` via `Unwrap`                          | `commitSession(sm, w, r)` BEFORE `datastar.NewSSE`                                                              |
| Templ import cycle (`web/templ ↔ internal/chat`)               | Compile error                                                                                | Define `MsgView`-style structs in `web/templ`; map in the handler                                                |
| FK constraint failed (787) when registering                    | invite consume `used_by` references not-yet-inserted user                                    | Insert user first, then consume invite, inside the same tx                                                       |
| Forgetting `templ generate` after editing `.templ`             | Compile error about undefined identifiers in `web/templ`                                     | `make gen` or `go tool templ generate`                                                                          |
| `Home` defined in both `home.templ` and `layout.templ`         | "redeclared in this block" after I moved `Home` but didn't delete the original               | When moving templ defs across files, delete BOTH the old `.templ` and the matching generated `_templ.go`         |
| Pushed `data-on:load`                                          | datastar v1 has no `load` DOM event                                                          | Use `data-init` for mount; `data-on:click`/`keydown`/`submit`/etc. for real DOM events                          |
| Using `datastar.WithModeAppend()` to build up a chat history    | DOM grows unbounded, scroll position becomes annoying                                        | Fat-morph the whole `#messages` + `sse.ExecuteScript` to scroll the container (§6)                              |
| Adding a separate `<div data-init="el.scrollIntoView()">` to trigger scroll | `data-init` doesn't re-fire on outer-morph of the same id; and the anchor is outside the scrollable container | Use `sse.ExecuteScript("document.querySelector('#messages')?.scrollTo(...)")` after every fat-morph |
| NATS publish payload = rendered HTML                            | Per-viewer state (e.g. mod buttons) baked into the wire payload, can't be right for everyone | Publish a tiny "changed" string; each subscriber refetches and renders for its own viewer                       |
| Smoke-testing on a busy port without checking                  | `bind: address already in use`, app dies during the test                                     | Use a fresh high port + `pkill -9 -f bin/forumchat` before each test cycle                                      |
| Trying to commit-amend after a pre-commit hook failed          | The commit didn't happen — amending modifies the PREVIOUS commit                             | Fix and create a NEW commit                                                                                     |
| Editing on `main`                                              | Pre-tool branch-check hook blocks                                                            | `git checkout -b task/<desc>` first                                                                             |

---

## 14. Things still on the roadmap

See `## Future` in
`eidos/spec - forumchat - community web app with realtime chat and forum threads.md`.
Highlights for whoever picks this up next:

- OAuth (Google → Facebook), linked to the existing global user.
- Multi-community UI (data model is already prepared).
- JetStream-backed chat with replay on reconnect (drops the "if NATS dies you
  miss messages until refresh" weakness).
- Custom modernc-backed `scs.Store` so sessions survive restart.
- Drag-drop upload UI (today's `POST /uploads` returns markdown text, manual
  paste).
- Search (SQLite FTS5).
- Trust levels (column already in `memberships`).
- Prometheus metrics endpoint.

---

## 15. Where to look for more

- `README.md` — user-facing project overview, env vars, routes, deploy.
- `eidos/spec - forumchat - ….md` — the spec; behaviour + design.
- `memory/plan - 2606131456 - ….md` — the implementation plan with a
  detailed progress log per phase. Read this for the "why we chose X" trail.
- `internal/auth/handlers.go` — reference implementation of the
  signals-driven SSE pattern (`PostLogin` shows the `commitSession` order).
- `internal/chat/handler.go` — reference for the fat-morph pattern.

If you're touching anything realtime — read §4 and §6 again before you type.
