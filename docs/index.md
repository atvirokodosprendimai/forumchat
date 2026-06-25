# Developer documentation

Build on top of a community. Forumchat is a single-binary community space —
realtime chat, threaded forums, video rooms, an optional AI assistant — and it
exposes a few small, sharp surfaces for wiring the outside world in.

This is the developer reference for those surfaces. Everything here is
first-party and compiled into the binary; nothing depends on an external CMS.

## Integration surfaces

| Surface | Shape | Use it for |
|---|---|---|
| [**Connectors**](/dev/docs/connectors) | persistent, bidirectional — a signed SSE stream + signed POSTs | running an outside program **as a human member**: a support desk, a shop concierge, a bridge to another chat network, a bespoke bot |
| [**Webhooks**](/dev/docs/webhooks) | stateless — one inbound `POST` in, one outbound `POST` per message out | fire-and-forget notifications, CI pings, Slack/Discord/Matrix bridges, badged bot messages |

### Which one do I want?

- You need to **subscribe to the live conversation and answer in it as a
  participant** → [**Connectors**](/dev/docs/connectors). The worker holds a
  stream open and appears in the roster, `@mention`-able, with a profile. This
  is the richer surface and has a [Go SDK](/dev/docs/connectors#go-sdk).
- You only need to **push a message in** or **get pinged on every message** →
  [**Webhooks**](/dev/docs/webhooks). No stream, no SDK, just HTTP.

Both are off by default and gated by an env flag
(`CONNECTORS_ENABLED`, `WEBHOOKS_ENABLED`); a community admin creates and
manages instances under `/c/{slug}/admin`.

## Conventions across the surfaces

- **Bearer-by-URL / bearer-by-body.** A read stream authenticates with a signed
  URL (no headers on an `EventSource`); a write authenticates with an HMAC over
  the exact request body. One secret powers both; rotating it revokes both.
- **Reveal once.** Secrets and signed URLs are shown a single time on create or
  rotate — copy them then.
- **JSON, not HTML.** These wires are for machines, so the payloads are JSON.
  (The browser UI is server-rendered HTML over a separate, datastar-driven
  channel — not something an integration touches.)

Start with [**External connectors**](/dev/docs/connectors).
