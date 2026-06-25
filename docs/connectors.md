# External connectors

A **connector** lets an arbitrary external program take part in a community's
chat **as if it were a person**. The worker holds open one signed
`GET` **SSE stream** to receive realtime messages, and **POSTs** signed
requests to send them back. From the community's point of view it's just another
member typing — it shows up in the roster, is `@mention`-able, has a profile,
can be replied to, and a moderator can delete its messages.

You can't know what sits on the far side — a custom chat app, a chatbot, a
shop concierge, a bridge to another network, a desk agent — and you shouldn't
have to. If you can open an HTTP stream and sign a request body, you can build
on this.

> **Connector vs webhook vs agent**
> - A [**webhook**](/dev/docs/webhooks) is a *stateless push*: a one-shot
>   inbound `POST` becomes a badged *bot* bubble; an outbound relay fires one
>   JSON `POST` per message. No live stream.
> - A **chat-agent** runs *our own* model in-process.
> - A **connector** is a *persistent, bidirectional session* running *someone
>   else's* logic out-of-process, and its messages are **human**, not badged.

---

## Quick start

1. Boot forumchat with **`CONNECTORS_ENABLED=true`** (read once at startup;
   with it off the admin page is hidden and `/bots/*` is unmounted).
2. Sign in as a community **admin** and open
   **`/c/{slug}/admin`** → **Connectors** (or go straight to
   `/c/{slug}/admin/connectors`).
3. **Create** a connector: give it a name (this becomes its member nick), tick
   the channels it should see (none = all non-archived channels), optionally set
   `mentions_only`, and grant the **capabilities** you want
   (`send`, and optionally `delete` / `ban` / `rename`).
4. On save the page **reveals once**: the `secret`, the signed **stream URL**,
   and the **send URL**. Copy the **id** and **secret** now — rotating
   re-reveals and invalidates the old ones.

You now have three things:

| Value | Where it lives | Secret? |
|---|---|---|
| `BASE_URL` | your forumchat origin, e.g. `https://chat.example.com` | no |
| connector `id` | in every URL (`/bots/<id>/…`) | no — it's public |
| connector `secret` | shown once on create / rotate | **yes** — treat like a password |

The fastest way to see it working end-to-end is the runnable
[`examples/tinychat`](https://github.com/atvirokodosprendimai/forumchat/tree/main/examples/tinychat)
terminal client (~200 lines) built on the [Go SDK](#go-sdk).

---

## Authentication

One per-connector `secret` (32 random bytes, base64url) powers both transports.
They authenticate differently because they can carry different things:

- **Read = a signed URL.** A long-lived `EventSource` can't set custom headers,
  so its credential rides *in the URL* as an HMAC signature bound to the
  connector `id` and an expiry.
- **Write = a body HMAC.** A `POST` can carry a header, so each request is
  signed over its exact body in `X-Signature` — tamper-proof.

**Rotating** the secret server-side mints a fresh one and invalidates the stream
URL *and* every old body signature at once. That's the single revoke lever.

### Stream signature

The `sig` query parameter on the stream URL is:

```
sig = hex( HMAC_SHA256( secret, id + "\n" + "stream" + "\n" + exp ) )
```

where `exp` is a Unix timestamp after which the URL stops working
(`exp=0` means non-expiring). The signature is bound to the `id`, so it can be
forged for neither another connector nor a later expiry.

### Body signature

Every `POST` (send and the moderation actions) carries:

```
X-Signature: sha256=<hex>     where  hex = hex( HMAC_SHA256( secret, rawBody ) )
```

The server recomputes the HMAC over the **raw request body** and compares it
constant-time. **Sign the exact bytes you send** — any re-marshalling,
whitespace change, or trailing newline will fail verification.

---

## Receiving messages — the stream

```
GET /bots/{id}/stream?exp=<unix>&sig=<hex>
Accept: text/event-stream
```

Public, no session, no CSRF — the signed URL *is* the bearer capability. The
response is a **raw `text/event-stream`** (JSON, not HTML — the consumer is a
machine). The stream is **live-only**: the watermark starts at connect time, so
you receive messages sent *after* you attach (no backlog replay).

It emits two event types.

### `event: ready` — the one-shot handshake

Sent once, first. A stateless worker can configure itself from it alone:

```json
{
  "connector": "Acme",
  "nick": "Acme",
  "channels": [
    { "id": "ch_01H…", "slug": "support", "name": "Support" },
    { "id": "ch_01J…", "slug": "general", "name": "General" }
  ]
}
```

### `event: message` — one per delivered chat message

```json
{
  "id": "msg_01H…",
  "channel": "support",
  "channel_id": "ch_01H…",
  "nick": "alice",
  "author_id": "usr_01H…",
  "kind": "user",
  "body_md": "hi @Acme can you help with order 4182?",
  "body_html": "<p>hi @Acme can you help with order 4182?</p>",
  "mentioned": true,
  "reply_to": "",
  "created_at": "2026-06-25T10:00:00Z",
  "attachments": [
    { "url": "https://chat.example.com/uploads/…", "mime": "image/png", "name": "receipt.png" }
  ]
}
```

| Field | Meaning |
|---|---|
| `id` | message id (pass to `delete`) |
| `channel` / `channel_id` | channel slug / stable id |
| `nick` | author's display name |
| `author_id` | stable user id (pass to `ban`); `""` for author-less rows |
| `kind` | `user` \| `webhook` \| `bot` |
| `body_md` / `body_html` | Markdown source / rendered, sanitized HTML |
| `mentioned` | `true` when the body `@mentions` **this** connector |
| `reply_to` | parent message id, when the message is a reply |
| `created_at` | RFC3339 UTC |
| `attachments[]` | directly fetchable, shared-signed `url` + `mime` + `name` |

**What you never receive:** your own posts (no echo), soft-deleted messages, and
`system` rows. With `mentions_only` set, you receive **only** messages whose
body `@mentions` your connector. A `:` comment heartbeat is sent every ~25 s so
idle proxies don't reap the stream.

### curl

```sh
BASE="https://chat.example.com"
ID="conn_01H…"
SECRET="paste-the-revealed-secret"
EXP=0   # non-expiring; or a future Unix timestamp

# sig = HMAC-SHA256(secret, "<id>\nstream\n<exp>")
SIG=$(printf '%s\nstream\n%s' "$ID" "$EXP" \
  | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')

curl -N "$BASE/bots/$ID/stream?exp=$EXP&sig=$SIG"
```

`-N` disables curl's buffering so frames print as they arrive. You'll see the
`ready` frame immediately, then a `message` frame per new chat message.

---

## Sending messages

```
POST /bots/{id}/send
Content-Type: application/json
X-Signature: sha256=<hmac of the raw body>
```

Body:

```json
{ "channel": "support", "body": "on it — what's the order id?", "reply_to": "" }
```

- `channel` is a **slug**. Omit it (or send `""`) only when the connector is
  subscribed to exactly one channel — then it defaults there; otherwise the
  server rejects an empty channel.
- `reply_to` is an optional parent message id; the parent must be a live message
  in the same channel.
- The connector must hold the **`send`** capability. A channel outside the
  connector's allowlist is rejected `403`.

On success: `200 { "id": "msg_…", "channel": "support" }`. The message fans out
exactly like a normal human send, so open browser tabs render it live.

### curl

```sh
BODY='{"channel":"support","body":"on it — what'\''s the order id?"}'

# X-Signature must be over the EXACT bytes sent below.
SIG=$(printf '%s' "$BODY" \
  | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')

curl -X POST "$BASE/bots/$ID/send" \
  -H "Content-Type: application/json" \
  -H "X-Signature: sha256=$SIG" \
  --data-raw "$BODY"
```

> Use `--data-raw` (not `-d`/`--data`, which strips newlines and can mangle
> `@`-prefixed values). The bytes signed by `printf '%s'` must be byte-identical
> to what curl sends, or the server returns `401`.

---

## Moderation actions

These act as the human member too, but each is gated by a capability the admin
grants per connector. A granted-but-unwired action returns `501`; a
not-granted one returns `403`. All take the same `X-Signature` body HMAC as
`send`.

### Delete a message — capability `delete`

```
POST /bots/{id}/delete     body: { "message_id": "msg_…" }
```

Soft-deletes a message (hidden from everyone). The target must be in one of the
connector's allowed channels.

```sh
BODY='{"message_id":"msg_01H…"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')
curl -X POST "$BASE/bots/$ID/delete" \
  -H "Content-Type: application/json" -H "X-Signature: sha256=$SIG" --data-raw "$BODY"
```

### Ban a member — capability `ban`

```
POST /bots/{id}/ban        body: { "user_id": "usr_…", "hours": 24 }
```

`hours: 0` is permanent. The server refuses to ban an admin/owner or the
connector itself. `user_id` is the `author_id` you saw on a stream `message`.

```sh
BODY='{"user_id":"usr_01H…","hours":24}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')
curl -X POST "$BASE/bots/$ID/ban" \
  -H "Content-Type: application/json" -H "X-Signature: sha256=$SIG" --data-raw "$BODY"
```

### Rename a channel — capability `rename`

```
POST /bots/{id}/rename     body: { "channel": "support", "name": "Customer Support" }
```

`channel` is the slug; the server refuses to rename the default `#general`.

```sh
BODY='{"channel":"support","name":"Customer Support"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | sed 's/^.* //')
curl -X POST "$BASE/bots/$ID/rename" \
  -H "Content-Type: application/json" -H "X-Signature: sha256=$SIG" --data-raw "$BODY"
```

---

## Go SDK

The [`sdk-go`](https://github.com/atvirokodosprendimai/forumchat/tree/main/sdk-go)
client is dependency-free (stdlib only) and signs every request for you. It
hands you plain structs, never DOM fragments.

```go
package main

import (
	"context"
	"fmt"
	"log"

	connector "github.com/atvirokodosprendimai/forumchat/sdk-go"
)

func main() {
	c := connector.New("https://chat.example.com", "conn_01H…", "the-secret")

	// Receive: Stream blocks for the life of the connection. It does NOT
	// reconnect on its own — wrap it in your own backoff loop.
	go func() {
		for {
			err := c.Stream(context.Background(), connector.Handlers{
				OnReady: func(r connector.Ready) {
					log.Printf("connected as %s — %d channels", r.Nick, len(r.Channels))
				},
				OnMessage: func(e connector.Event) {
					fmt.Printf("#%s @%s: %s\n", e.Channel, e.Nick, e.BodyMD)
					if e.Mentioned {
						_, _ = c.Reply(context.Background(), e.Channel, "👋 hi!", e.ID)
					}
				},
			}, 0 /* exp: 0 = non-expiring URL */)
			log.Printf("stream ended: %v — reconnecting", err)
			// add a backoff/sleep here in real code
		}
	}()

	// Send.
	if _, err := c.Send(context.Background(), "support", "hello from the outside"); err != nil {
		log.Fatal(err)
	}
	select {}
}
```

The client also exposes `Reply`, `Delete`, `Ban`, and `Rename`, each requiring
the matching capability. Errors from any call are an `*connector.APIError`
carrying the HTTP status and the server's short message — switch on
`.Status` to branch on the failure modes below.

---

## Reconnecting

`Stream` returns on a clean server close (`nil`), on caller cancel (`ctx.Err()`),
or on a transport error. **It never reconnects itself** — that policy is yours.
A minimal robust loop:

- reconnect on any return,
- back off (e.g. 1s → 2s → … capped at 30s) and reset the backoff after a
  successful `ready`,
- because the stream is **live-only**, assume you may have missed messages while
  disconnected; if you need exactly-once semantics, reconcile against your own
  store on reconnect.

The SDK exports `connector.ErrFrameTooLarge` so your loop can distinguish a
misbehaving peer from an ordinary drop.

---

## Errors

| HTTP | When |
|---|---|
| `404` | unknown or disabled connector, or a bad/missing **stream** signature (anti-enumeration — the URL simply "doesn't exist") |
| `401` | bad or missing **send** body signature (`X-Signature`) |
| `403` | capability not granted, or channel outside the connector's allowlist |
| `400` | malformed request (bad JSON, unknown channel slug, empty channel with >1 subscription) |
| `501` | capability granted but the server action is not wired |

---

## Security notes

- The `secret` is the only credential — store it like a password, never in a
  URL query string you log, never client-side in a browser.
- Prefer an **expiring** stream URL (`exp` = a near-future timestamp) for
  short-lived workers; the SDK rebuilds a fresh signed URL on each `Stream`
  call, so a reconnect loop can roll the expiry forward.
- **Rotate** the secret the moment it might have leaked — it revokes the stream
  URL and all body signatures simultaneously.
- The synthetic member behind a connector can never log in and is auto-approved;
  deleting the connector removes that member (its authored messages survive as a
  "deleted member", like account erasure).
