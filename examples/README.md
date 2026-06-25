# forumchat integration examples

Wiring external systems to a community. Two integration surfaces:

- **Webhooks** (`WEBHOOKS_ENABLED=true`) — stateless pushes: an inbound `POST`
  becomes a badged *bot* message; an outbound relay fires one JSON `POST` per
  message. The rest of this document covers webhooks; see `eidos/spec - webhooks
  - …`.
- **Connectors** (`CONNECTORS_ENABLED=true`) — a *persistent, bidirectional*
  session where an external worker holds open a **signed SSE stream** and POSTs
  back, appearing as a **human member** (roster, `@mention`, profile, reply).
  See **[`tinychat/`](tinychat/)** for a runnable Go example built on the
  [`sdk-go`](../sdk-go) client, and `eidos/spec - connectors - …` for the design.

## The webhook contract

| Direction | What forumchat exposes / sends |
|---|---|
| **Inbound** (external → community) | `POST https://<host>/hooks/<token>` — the token is the secret. Body is parsed by the webhook's provider adapter. Posts as a named **bot** message into the configured channel. |
| **Outbound** (community → external) | forumchat `POST`s a JSON body to the webhook's `target_url` on every human chat message in the chosen channel. |

Create webhooks at **`/c/<slug>/admin/webhooks`** (community admin only). The
inbound URL is shown once on create — copy it then.

### Inbound payload shapes (what to POST to `/hooks/<token>`)

- **`generic`** provider — `{"text": "..."}` **or** `{"content": "..."}`. Any
  other JSON (or non-JSON) is posted verbatim in a code block. This is the
  catch-all: Slack-outgoing, Discord, Matrix bridges, CI, scripts.
  - **Forum-thread routing (optional):** add `"thread_key": "<stable id>"` and
    the message lands in the **forum** instead of the chat channel. The first
    message for a given `thread_key` opens a thread (titled by `"subject"`, or
    the first line); later messages with the same key append posts. `"author"`
    sets the far-side human's display name on the post. The response is
    `{"thread_id": "...", "post_id": "..."}` so a bridge can store the reverse
    mapping. Omit `thread_key` for the normal chat-bot behaviour.
- **`github`** provider — point a GitHub repo/org webhook here (content-type
  `application/json`). `push` / `pull_request` / `issues` / `release` are
  formatted; set the GitHub webhook **secret** to the webhook's signing secret
  to enable HMAC verification (`X-Hub-Signature-256`).

Quick test of any inbound `generic` webhook:

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d '{"text": "hello from curl :wave:"}' \
  https://your-forumchat-host/hooks/PASTE_TOKEN_HERE
```

### Outbound payload shapes (what forumchat sends to `target_url`)

- **`slack`** / **`discord`** provider → `{"text": "[#channel] author: body"}`
- **`generic`** provider → `{"community","channel","author","body_md","created_at"}`
  - **Forum-thread relays** also carry the thread identity:
    `{..., "thread_id", "subject", "thread_root", "message_id"}`. `thread_root`
    is `true` for the message that opened the thread (then `message_id` is
    omitted); `false` for replies (then `message_id` is the post id). Chat
    relays omit all four keys. Use `thread_id` to group messages into one
    external thread.

Only human (`kind=user`) messages are relayed — bot/system/forward messages are
not, so an inbound post never loops back out.

## Matrix

Matrix is **not** a webhook itself, so it's bridged at the webhook boundary, and
the two directions use different tools:

| Direction | Tool | This folder |
|---|---|---|
| forumchat → Matrix | **maubot** incoming-webhook plugin (HTTP → room) | `maubot/` ✅ |
| Matrix → forumchat | **matrix-hookshot** outbound webhook (room → HTTP) | see below |

A native Matrix bridge (Application Service — federation, puppeting) is out of
scope; these two cover the practical cases.

### forumchat → Matrix (maubot) — see `maubot/`

A forumchat **outbound** webhook (`provider: generic`) points its `target_url`
at a maubot [`maubot-webhook`](https://github.com/jkhsjdhjs/maubot-webhook)
plugin instance, which posts the message into a Matrix room. Config + steps in
[`maubot/`](./maubot/).

### Matrix → forumchat (matrix-hookshot)

maubot's webhook plugins are receivers (HTTP → Matrix), so they can't push
*out* of a room. For room → forumchat use
[matrix-hookshot](https://matrix-org.github.io/matrix-hookshot/latest/setup/webhooks.html)
**outbound webhooks**: configure an outbound hook on the room with the URL set
to your forumchat inbound `generic` endpoint `https://<host>/hooks/<token>`.

Hookshot's default outbound body is its own JSON envelope, which the `generic`
adapter posts verbatim (fenced). For a clean one-line message, give hookshot a
transformation function that emits `{"text": ...}`:

```js
// hookshot outbound transformation (JS)
result = { text: `${data.sender}: ${data.content?.body ?? ''}` };
```

### Thread sync (forumchat forum thread ↔ Matrix thread)

forumchat speaks both halves of the thread contract (see the payload shapes
above): outbound forum relays carry `thread_id` + `thread_root`, and the
inbound `generic` endpoint routes a message into the forum when it carries
`thread_key`. What forumchat does **not** ship is the piece that maps the two
id-spaces, because that is inherently stateful and Matrix-side:

- **forumchat → Matrix** needs a consumer that remembers
  `forumchat thread_id → Matrix thread-root event` and posts each relay as an
  `m.thread` reply under that root (creating the root on `thread_root: true`).
- **Matrix → forumchat** needs a producer that sends a Matrix thread's
  root-event id as `thread_key` (and the human's name as `author`), then stores
  the `{thread_id}` forumchat returns so the reverse direction can find it.

The **stock** `maubot-webhook` plugin and `matrix-hookshot` are stateless,
template-only, and **m.thread-unaware** — they can mirror flat channel↔room
traffic (above) but cannot map threads. True thread sync requires a small
**custom, stateful maubot plugin** that holds the id↔event map and reads/writes
the `thread_*` fields. forumchat's side of that contract is in place; the plugin
is the remaining piece (not shipped here — a native Matrix Application Service
bridge is the heavier alternative, still out of scope).
