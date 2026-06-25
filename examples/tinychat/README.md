# tinychat — external chat app over a forumchat connector

A ~200-line terminal chat client that joins a community **as an external
connector**: it opens the signed SSE stream to print messages live and sends
whatever you type back into the channel as the connector's (human-looking)
member. It's the smallest real demonstration of the
[`sdk-go`](../../sdk-go) connector client.

```
14:02 #support ● connected as Acme — channels: #support #general
14:02 #support @alice  hi @Acme can you help?
14:03 #support you   on it — what's the order id?
```

## 1. Create a connector (admin GUI)

1. Boot forumchat with **`CONNECTORS_ENABLED=true`** (the flag is read once at
   startup; with it off the page is hidden and `/bots/*` is unmounted).
2. Sign in as a community **admin**.
3. Open **`/c/{slug}/admin`** → click **Connectors** (next to Webhooks / Mail
   filters), or go straight to **`/c/{slug}/admin/connectors`**.
4. **Create** one: give it a name (this becomes its member nick), tick the
   channels it should see (none = all), optionally `mentions_only`, and grant
   the **send** capability (plus `delete`/`ban`/`rename` if you want it to
   moderate).
5. On save the page **reveals once**: the `secret`, the signed **stream URL**,
   and the **send URL**. Copy the **id** and **secret** now — rotate re-reveals.

## 2. Run

```sh
cd examples/tinychat
go run . \
  -base   https://chat.example.com \
  -id     <connector id> \
  -secret <connector secret> \
  -channel support          # optional; omit to use the connector's sole channel
```

Flags fall back to env: `BASE_URL`, `CONNECTOR_ID`, `CONNECTOR_SECRET`,
`CHANNEL`. Set `NO_COLOR=1` to disable ANSI colour.

- Type a line + Enter to **send**. Your own line is echoed locally (the stream
  never echoes the connector's own messages).
- A message that **@mentions** the connector is highlighted.
- **`/quit`** or **Ctrl-C** exits cleanly.

## 3. What it shows about the API

| SDK call | Used for |
|---|---|
| `connector.New(base, id, secret)` | bind a client to one connector |
| `c.Stream(ctx, Handlers{OnReady, OnMessage}, 0)` | the long-lived **read** stream (signed URL built + signed for you) |
| `c.Send(ctx, channel, body)` | **write** a message as the member (body-HMAC signed) |

The reconnect-with-backoff loop lives in the app, not the SDK — `Stream` is
one-shot on purpose so the policy stays visible (see `streamLoop` in
`main.go`). The moderation calls (`c.Delete`, `c.Ban`, `c.Rename`) aren't
exercised here; they're one line each and need the matching capability granted
on the connector.

## Notes

- The connector **acts as a human** — it shows in the roster, is `@mention`-able,
  has a profile, and a mod can delete its messages. No bot badge.
- This example is its own Go module with a local `replace` pointing at
  `../../sdk-go`. A real external app would
  `go get github.com/atvirokodosprendimai/forumchat/sdk-go` and drop the replace.
- Wire format is **JSON**, not datastar/HTML — a connector's consumer is a
  machine, so the SDK hands you plain structs.
