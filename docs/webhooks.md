# Webhooks

A **webhook** is the *stateless* integration surface — the simpler sibling of
[connectors](/dev/docs/connectors). There's no live stream and no SDK: an
inbound `POST` becomes a badged **bot** message, and an outbound relay fires one
JSON `POST` per human chat message. Enable with **`WEBHOOKS_ENABLED=true`** and
create instances at **`/c/{slug}/admin/webhooks`** (community admin only). The
inbound URL is revealed once on create — copy it then.

| Direction | What forumchat does |
|---|---|
| **Inbound** (external → community) | `POST https://<host>/hooks/<token>` — the token in the path is the secret. Posts as a named **bot** message into the configured channel. |
| **Outbound** (community → external) | forumchat `POST`s a JSON body to the webhook's `target_url` on every human chat message in the chosen channel. |

---

## Inbound

`POST https://<host>/hooks/<token>`. The body is parsed by the webhook's
**provider** adapter.

### `generic` provider

The catch-all — Slack-outgoing, Discord, CI, scripts. Send either shape:

```json
{ "text": "deploy finished ✅" }
```
```json
{ "content": "deploy finished ✅" }
```

Any other JSON (or non-JSON) is posted verbatim in a code block.

```sh
curl -X POST "https://chat.example.com/hooks/$TOKEN" \
  -H "Content-Type: application/json" \
  --data-raw '{"text":"deploy finished ✅"}'
```

**Forum-thread routing (optional).** Add a `thread_key` and the message lands in
the **forum** instead of the chat channel. The first message for a given
`thread_key` opens a thread (titled by `subject`, or the first line); later
messages with the same key append posts. `author` sets the far-side display
name. The response is `{ "thread_id": "…", "post_id": "…" }` so a bridge can
store the reverse mapping.

```json
{
  "thread_key": "ticket-4182",
  "subject": "Order 4182 — refund requested",
  "author": "Zendesk",
  "text": "Customer asks for a refund on order 4182."
}
```

### `github` provider

Point a GitHub repo/org webhook at `/hooks/<token>` with content-type
`application/json`. `push` / `pull_request` / `issues` / `release` events are
formatted into readable messages. Set the GitHub webhook **secret** to the
webhook's signing secret to enable HMAC verification
(`X-Hub-Signature-256`).

---

## Outbound

When a human posts in the webhook's channel, forumchat `POST`s a JSON body to
the configured `target_url`. Use it to mirror chat into another system. Build
the receiver to respond `2xx` quickly and do slow work asynchronously — the
relay is fire-and-forget and does not retry indefinitely.

> URLs in a relayed message are absolute. Relative `/uploads` or `/c` paths are
> rewritten to absolute URLs so a downstream bridge (Matrix, etc.) can fetch
> attachments.

---

## Webhook or connector?

- Need a **live conversation** you answer in **as a member** → use a
  [**connector**](/dev/docs/connectors).
- Need **fire-and-forget** push in/out → webhooks are all you need.
