# maubot ↔ forumchat

Bridge a forumchat channel into a Matrix room with [maubot](https://github.com/maubot/maubot).

## forumchat → Matrix (working, off-the-shelf)

Posts every human chat message from a forumchat channel into a Matrix room.

Pieces:

```
forumchat channel  --(outbound webhook, provider: generic)-->  maubot-webhook plugin  -->  Matrix room
```

### 1. Install the plugin in maubot

Use [`jkhsjdhjs/maubot-webhook`](https://github.com/jkhsjdhjs/maubot-webhook)
(incoming HTTP → Matrix room). Upload the `.mbp`, create an **instance**, and
paste [`forumchat-to-matrix.base-config.yaml`](./forumchat-to-matrix.base-config.yaml)
as its config. Set:

- `room` — your Matrix room's internal ID (`!…:server`).
- the instance must use a bot account that has joined that room.

maubot exposes the instance at:

```
https://<your-maubot-host>/_matrix/maubot/plugin/<instance-id>/forumchat
```

### 2. Create the forumchat outbound webhook

In forumchat → **`/c/<slug>/admin/webhooks`** → create:

- **Direction:** Outbound
- **Provider:** `generic`  (richer payload; or `slack` for a flat `{text}` — see the config's alternative block)
- **Source channel:** the channel to relay (or "All channels")
- **Target URL:** the maubot endpoint from step 1

Send a message in that forumchat channel → it appears in the Matrix room.

### Auth note

forumchat's outbound relay sends **no auth header** in v1. Keep maubot's
`auth_type`/`auth_token` null; the unguessable maubot instance path is the
practical control (front it with a reverse proxy if you need a shared secret).

## Matrix → forumchat

maubot's webhook plugins only **receive** HTTP (HTTP → room), so they can't push
a room's messages out. For Matrix → forumchat use **matrix-hookshot** outbound
webhooks pointed at your forumchat inbound `generic` endpoint
(`https://<host>/hooks/<token>`) — details in [`../README.md`](../README.md).
