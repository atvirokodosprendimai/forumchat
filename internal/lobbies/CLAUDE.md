# lobbies

Tokenised guest access. Admin/mod mints `/lobby/<token>` URL; guest joins with
a chosen name, signed cookie binds the browser. Persistent chat (`lobby_messages`
table, mirrors `chat_messages` shape), image uploads piped through
`uploads.Store` with synthetic `lobby:<id>` user id, NATS subject
`community.<cid>.lobby.<lid>`, in-process per-lobby Bus, push notifications
for the host on guest send (`lobby_message` event kind).

Gated by `GUEST_ACCESS_ENABLED=true`. Sidebar item visible to admin/mod only.
