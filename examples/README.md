# forumchat webhook integration examples

Wiring external systems to a community via webhooks (`WEBHOOKS_ENABLED=true`).
See `eidos/spec - webhooks - …` for the full design.

## The contract

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
