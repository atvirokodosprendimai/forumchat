# forumchat-outbound

A [maubot](https://github.com/maubot/maubot) plugin that bridges Matrix room
messages **outbound** to a forumchat inbound `generic` webhook — the reverse of
[jkhsjdhjs/maubot-webhook](https://github.com/jkhsjdhjs/maubot-webhook) (which
brings forumchat messages **into** Matrix).

Together the two give a clean, maubot-only **bidirectional** bridge — no
matrix-hookshot required.

## What it does

Listens for `m.room.message` events in configured rooms and POSTs them to a
forumchat webhook as:

```json
{"text": "<sender>: <body>"}
```

forumchat's inbound `generic` provider accepts `{"text": ...}` (or
`{"content": ...}`), so the message posts as a named bot message in the channel.

## Loop guard

Without filtering, the two plugins would echo forever:

1. Someone posts in forumchat → forumchat→Matrix bot posts in the room.
2. forumchat-outbound sees that post → POSTs it back to forumchat.
3. Repeat.

`ignore_senders` breaks the loop — set it to the MXID of your forumchat→Matrix
bot (the maubot-webhook instance's client). forumchat also dedupes inbound
messages, giving a second layer.

## Encrypted rooms (important)

If your room is end-to-end encrypted, the bot's maubot **client must have a
crypto identity** or it cannot read (decrypt) human messages and nothing is
relayed. When creating the client, log in with a device and pass both the
`access_token` and `device_id` (and `homeserver`). A client showing
`device_id: ""` / `fingerprint: null` has no crypto. maubot's image ships
`python-olm`, so once a device is set it uploads keys and decrypts new messages.

## Configuration

| Key | Default | Description |
|-----|---------|-------------|
| `target_url` | `""` | forumchat inbound generic webhook URL (`https://<host>/hooks/<token>`). Blank = disabled (fail-safe). |
| `rooms` | `[]` | Matrix room IDs to relay. Empty = relay nothing (fail-safe). |
| `ignore_senders` | `["@bot.maubot:example.org"]` | MXIDs never relayed (loop guard). |
| `allowed_msgtypes` | `["m.text", "m.emote"]` | Message types to relay. |
| `format` | `"{sender}: {body}"` | Output template. Placeholders: `{sender}`, `{body}`. |
| `use_display_name` | `true` | Use room display name for `{sender}`, else MXID localpart. |

## Limitations

- **Text only.** forumchat's inbound `generic` webhook accepts a text payload,
  so only `m.text`/`m.emote` are relayed. Images and other media (`m.image`,
  `m.file`, …) are not bridged outbound.

## Build & install

```sh
zip -r -X forumchat-outbound.mbp maubot.yaml base-config.yaml forumchat_outbound
```

Upload the `.mbp` in the maubot management UI (or via the API), create an
instance using the client that is joined to your room, and set `target_url` +
`rooms`.

## Tests

```sh
python3 -m pytest tests -q
```

Pure relay logic (`logic.py`) is tested without the maubot runtime.
