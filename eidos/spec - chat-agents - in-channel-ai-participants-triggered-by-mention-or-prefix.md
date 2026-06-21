---
name: spec-chat-agents
status: draft
type: spec
tldr: Bring-your-own-agent for the live chat channel. The existing per-community `ai_agents` (name/model/system-prompt) become first-class chat participants — shown in the roster with a bot icon, @mentionable, triggered in-line by @mention OR a configurable line-prefix (default '.') OR every message ('all'), scoped to admin-assigned channels. The bot streams its answer as a live-typing `kind='bot'` bubble built from the last ~30 channel messages, reusing the agent runner/provider. Gated by AI_ENABLED + per-agent `in_chat_enabled`.
---

# Chat Agents — in-channel AI participants

A community already has **agents** (`ai_agents`: unique name, provider, model,
system prompt, MCP tools) reachable as a separate ChatGPT-style pane at
`/c/{slug}/agent`. This spec makes those same agents **participants in the live
chat channel**: they appear in the online roster wearing a bot icon, they are
`@mention`-able like a member, and they answer in-line when triggered — so a
member just types `@nick …` (or `.nick …`) in `#general` and nick replies in the
channel as a streaming bubble.

This is **"bring your own agent"**: an admin adds N agents, each with its own
model + rules (system prompt) + name, and flips a per-agent switch to let it
join chat.

## Target

The gap: AI is sequestered in a separate pane. Members in the flow of a chat
conversation cannot pull the assistant *into the room* — they have to leave,
open the agent pane, lose the channel context, and copy answers back (the
existing `ShareToChannel` is a one-way export, not a conversation). Communities
asked for an ambient assistant that behaves like a regular member: visible in
the roster, addressable by name, answering where the conversation already is.

This reverses two earlier, deliberate decisions — surface them, don't bury them:

- **The agent pane was isolated *because chat has no thread context*** ([[spec - agent - per-community-ai-chat-with-threads-and-resumable-streaming]]).
  We resolve this by feeding the bot the **last N channel messages** as its
  context window — channel-as-conversation instead of an explicit thread.
- **Webhook bots were made deliberately non-@mentionable**
  ([[spec - webhooks - inbound-bot-messages-and-outbound-event-relay]]). Chat
  agents are the opposite: being @mentionable IS the trigger. We keep them as a
  **distinct `kind='bot'`** so the webhook bubble's affordance suppression is
  untouched.

Both surfaces coexist: the agent pane stays for deliberate, threaded, private
AI sessions; chat agents are the ambient, public, in-channel assistant.

## Behaviour

### Feature flag

- Reuses the existing instance flag `AI_ENABLED`. No new instance flag in v1.
- Per-agent column `ai_agents.in_chat_enabled` (default `0`) gates a single
  agent's chat participation independently of its pane availability
  (`ai_agents.enabled`). An agent can be pane-only, chat-only, or both.

### Trigger — resolved decision: per-agent, configurable

Each agent row declares **how** it is triggered:

```
ai_agents.trigger_mode   TEXT NOT NULL DEFAULT 'mention'
                         CHECK (trigger_mode IN ('mention','prefix','both','all'))
ai_agents.trigger_prefix TEXT NOT NULL DEFAULT '.'
```

After a member's `kind='user'` message persists and fans out, the chat handler
calls `chatagents.Dispatch(ctx, communityID, channelID, msg)`. For each enabled
in-chat agent **bound to this channel**, the matcher decides:

- **`mention`** — body contains `@<name>` (case-insensitive, word-boundaried;
  reuses the existing mention parse). The headline path.
- **`prefix`** — a line starts with `trigger_prefix`. When >1 prefix-agent
  shares a channel, the match requires `<prefix><name>` (e.g. `.nick …`) to
  disambiguate; a lone prefix-agent matches a bare `<prefix> …`.
- **`both`** — either of the above.
- **`all`** — every non-bot message in the channel (the "dedicated #ask-ai
  channel" shape, expressed per-agent instead of per-channel). Use sparingly.

A message can trigger more than one agent (e.g. `@nick @docs compare…`); each
runs independently. The triggering text is passed through verbatim — the bot
sees the `@nick`/`.nick` token, the system prompt tells it that's its name.

### Loop guard

`Dispatch` is a **no-op** when the triggering message's `kind` is anything but
`user` — `bot`, `webhook`, `system`, and `thread_announce` never trigger an
agent. **Bot-to-bot conversation is disabled in v1** (an agent cannot address
another agent). This is the single rule that makes auto-response safe; it has no
exceptions in v1.

### Channel scope — resolved decision: admin-assigned

A join table binds agents to channels:

```
ai_agent_channels (
  agent_id   TEXT NOT NULL REFERENCES ai_agents(id)      ON DELETE CASCADE,
  channel_id TEXT NOT NULL REFERENCES chat_channels(id)  ON DELETE CASCADE,
  PRIMARY KEY (agent_id, channel_id)
)
```

An agent appears in the roster + mention autocomplete **only** for its bound
channels, and `Dispatch` only considers agents bound to the active channel.
Empty binding = participates nowhere (chat-disabled in effect).

### Delivery — resolved decision: live streaming bubble

Reuses the agent runner (`internal/agent/runner.go`) machinery — provider
interface, detached goroutine, `time.Ticker(100ms)` flush, agentic MCP loop —
but writes into **chat** instead of `ai_messages`:

1. Insert a placeholder `kind='bot'` `chat_messages` row:
   `gen_status='generating'`, `body_md=''`, `bot_name=agent.name`,
   `bot_avatar_url=agent.avatar_url`, `bot_agent_id=agent.id`, `channel_id`.
2. The generation goroutine streams tokens into a buffer; every 100ms it
   rewrites that row's `body_md` and broadcasts the channel id. Open chat SSE
   streams refetch + fat-morph `#messages` — the existing chat fan-out renders
   the growing bubble with a typing cursor (`▍`) while `gen_status='generating'`.
3. On completion `gen_status='done'`, final broadcast. The bubble drops the
   cursor and gains the mod **delete** affordance (same as a webhook bubble).
4. **Resumable / honest restart** (inherited from the agent spec): the DB is the
   single source of truth, so a refresh re-renders the partial and continues
   live. A *server* restart flips lingering `gen_status='generating'` bot rows
   to `interrupted` on boot; the bubble offers **Regenerate**.
5. **One generation per (agent, channel)** at a time — reuses the runner's
   `active` map keyed by a synthetic id `chat:<channelID>:<agentID>`. A second
   trigger while busy is dropped (not queued) in v1.

### Context window — resolved decision: last ~30 channel messages

`buildChannelHistory` loads the last `ChatAgentContextLimit` (≈30) **non-bot**
messages in the channel (reusing `chat.Repo.Recent`) and maps them to provider
turns:

- The agent's own prior `kind='bot'` messages (matched by `bot_agent_id`) →
  role `assistant`.
- Everyone else's messages → role `user`, prefixed with the author's display
  name so the model can attribute who said what.
- `system` prompt = `agent.system_prompt` ("rules"), plus a small preamble
  stating the agent's name and that it is one participant in a shared channel.

No thread replay (chat has no threads); a `reply_to_id` on the triggering
message MAY be prepended as extra context (future refinement, not v1).

### Identity — `kind='bot'`, mentionable, bot icon

A new chat message kind keeps webhook semantics intact while flipping the
affordances chat agents need:

- New `chat.Kind` `KindBot = "bot"`; new `webtempl.MsgKind` `MsgKindBot`.
- Reuses the existing denormalised `chat_messages.bot_name` / `bot_avatar_url`
  columns (added by [[spec - webhooks - inbound-bot-messages-and-outbound-event-relay]]),
  plus a new `bot_agent_id TEXT REFERENCES ai_agents(id) ON DELETE SET NULL` for
  provenance (render link, loop attribution, regenerate).
- `MsgView` bubble for `MsgKindBot`: avatar + name + body **like a user bubble**,
  exposes **no** PM/profile/reply-to-author affordances (no `author_id`), but
  **is a valid @mention target** and shows a 🤖 badge. Mods can delete. Cannot
  be bookmarked-to-DM or promoted-to-thread.
- **Roster:** synthetic always-online entries. `presence.RosterMember` gains
  `IsBot bool`; the roster handler appends one entry per in-chat agent bound to
  the viewer's active channel, rendered with the bot icon, sorted among members.
  Clicking a bot roster row opens no profile/PM menu in v1.
- **Mention autocomplete:** `/chat/mention` unions member results with the
  in-chat agent names bound to the channel, so `@n…` surfaces `nick`.

### Admin CRUD

Extends the existing admin → AI agent editor (`web/templ/agent.templ` admin
section) — no new admin page:

- **Participate in chat** checkbox → `in_chat_enabled`.
- **Avatar URL** → new `ai_agents.avatar_url` (the pane never needed one;
  chat does).
- **Trigger** `<select>` (`mention` | `prefix` | `both` | `all`) +
  **prefix** `<input>` (shown when mode ≠ `mention`, default `.`).
- **Channels** multi-select (writes `ai_agent_channels`).

A roster/autocomplete cache per community is invalidated (and `presence.Bump`
fired) on any of these admin mutations so open chat tabs re-render live.

## Boundaries (explicitly NOT)

- **No bot-to-bot.** The loop guard refuses any non-`user` trigger; agents never
  converse with each other or with webhooks in v1.
- **No DMs to a bot.** Channel participation only; the roster bot row has no PM
  affordance.
- **No forum / projects participation.** Chat channels only. (Forum already has
  chat-promotion; that is unrelated.)
- **Not the agent pane.** This does not change `/c/{slug}/agent` threading,
  visibility, or its private/shared model.
- **No queue for concurrent triggers.** One generation per (agent, channel);
  extra triggers while busy are dropped, not queued.
- **No per-message model override.** The agent's configured model is used.

## Design

| Layer | File |
|---|---|
| Schema | `internal/storage/sqlite/migrations/00043_chat_agents.sql` — ALTER `ai_agents` (+`in_chat_enabled`,`trigger_mode`,`trigger_prefix`,`avatar_url`); new `ai_agent_channels`; ALTER `chat_messages` (+`bot_agent_id`,`gen_status`) |
| Trigger matcher (pure) | `internal/chatagents/match.go` — `(agent, msg) → bool`, table-tested |
| Dispatch + channel-scope reads | `internal/chatagents/dispatch.go` |
| Channel-history build + runner adapter | `internal/chatagents/runner.go` (reuses `internal/agent` provider + flush loop) |
| New kind render | `internal/chat/chat.go` (`KindBot`), `web/templ/chat.templ` (`MsgKindBot` bubble) |
| Roster bot entries | `internal/presence/handler.go`, `web/templ/roster.templ` (`IsBot`) |
| Mention union | `internal/chat/handler.go` (`/chat/mention`) |
| Admin form | `web/templ/agent.templ` admin section |

Why a new package `internal/chatagents` and not a method on `chat` or `agent`:
- It imports **both** `chat` (write the bubble, fan-out) and `agent` (provider,
  runner) — putting it in either creates a cycle. `chatagents` is the seam,
  exactly like `projects.PostExtractFromChat` lives outside `chat`
  (`internal/chat/CLAUDE.md §6.7`).

NATS: reuses the existing chat subject `community.<cid>.chat` (payload =
channel id), so streaming bot bubbles ride chat's existing fan-out with zero
new subject. No `community.<cid>.agent.thread.<tid>` here — that is the pane's.

### Read-path touch (the one cost)

`chat_messages` gains `bot_agent_id` + `gen_status`; every chat scan
(`Recent`, `listBefore`, `ByID`) carries the two columns — the same kind of
hot-path touch webhooks already made for `bot_name`/`bot_avatar_url`. The path
stays JOIN-free: the bot's identity is denormalised on the row.

## Verification

- **Matcher unit table** (`match_test.go`): mention hit/miss with word
  boundaries + case; prefix lone vs `<prefix><name>` disambiguation; `both`;
  `all`; loop-guard skip for every non-`user` kind.
- **Dispatch**: given a channel binding, only bound+enabled agents are
  considered; an `AI_ENABLED=false` instance dispatches nothing.
- **Runner** (stub Ollama, like `agent_test.go`): placeholder bot row →
  100ms flush rewrites `body_md` → `gen_status='done'`; the interrupt sweep on
  boot flips a stranded `generating` bot row to `interrupted`.
- **Identity / render**: a `kind='bot'` bubble shows avatar+name+🤖, exposes no
  PM/profile/reply, IS a mention target, a mod can delete it.
- **Roster**: an in-chat agent bound to `#general` appears online with the bot
  icon for a viewer in `#general`, absent in a channel it isn't bound to.
- **E2E smoke**: `AI_ENABLED=true`, one agent `nick` (`trigger_mode='both'`,
  bound to `#general`). `@nick hi` → streaming bot bubble; `.nick hi` → same;
  a plain message → no response; a webhook post → no response (loop guard).

## Friction

- **Streaming bubble fan-out cost.** Every 100ms flush fat-morphs `#messages`
  for *every* open tab in the channel (chat is community-wide fan-out). Brotli
  compresses the repeated full-conversation patch ~20× (per the agent spec), so
  acceptable at target scale; a very large channel with several concurrent bot
  generations is the worst case. No per-token wire frames — same trade as chat.
- **Prefix collisions.** Two `prefix`-mode agents sharing `.` in one channel
  must be addressed `.name`; a bare `.` is ambiguous and matches none. Documented
  in the admin help text.
- **`all`-mode noise.** An `all` agent answers every message; intended for a
  dedicated channel, footgun in `#general`. Admin-only to set; warn in UI.
- **No concurrent-trigger queue.** Rapid re-triggers while a bot is mid-answer
  are dropped. Acceptable for v1; JetStream-backed queue is future (mirrors the
  agent/chat replay roadmap).
- **Cost / abuse.** Any member can summon any bound bot; an `AI_ENABLED`
  instance with open registration could be driven to run the model arbitrarily.
  v1 leans on per-agent `in_chat_enabled` + channel binding + admin trust; a
  rate limit per (member, agent) is future.

## Interactions

- Reuses the engine of [[spec - agent - per-community-ai-chat-with-threads-and-resumable-streaming]]
  (provider, runner, 100ms flush, interrupt-on-boot, MCP tools) and its
  `ai_agents` table (name/model/system prompt = the "bring your own agent" data).
- Reuses the bot-identity columns + `kind` machinery of
  [[spec - webhooks - inbound-bot-messages-and-outbound-event-relay]] but adds a
  distinct `kind='bot'` so webhook non-mentionability is preserved.
- Depends on [[spec - chat-channels - admin-curated-public-text-channels]] for
  the per-channel binding (`ai_agent_channels.channel_id`) and the channel-scoped
  roster/autocomplete.
- Extends [[spec - forumchat - community web app with realtime chat and forum threads]]
  chat write path + roster.

## Mapping

> [[internal/storage/sqlite/migrations/00043_chat_agents.sql]]
> [[internal/chatagents/match.go]]
> [[internal/chatagents/dispatch.go]]
> [[internal/chatagents/runner.go]]
> [[internal/chat/chat.go]]
> [[internal/chat/handler.go]]
> [[internal/agent/runner.go]]
> [[internal/presence/handler.go]]
> [[web/templ/chat.templ]]
> [[web/templ/roster.templ]]
> [[web/templ/agent.templ]]

## Future

- {[?] Phase 1 — migration + `kind='bot'` render + roster bot entries + mention union (no generation yet; static bot identity).}
- {[?] Phase 2 — matcher + Dispatch + channel-history build + runner adapter streaming the bubble; loop guard.}
- {[?] Phase 3 — admin form (in_chat toggle, trigger mode/prefix, channel multi-select, avatar) + cache invalidation.}
- {[?] reply_to_id parent prepended to context.}
- {[?] Per-(member, agent) rate limit + abuse controls.}
- {[?] Concurrent-trigger JetStream queue (vs drop).}
- {[?] DM a bot (private 1:1 with an agent, distinct from the threaded pane).}
- {[?] Tool-call chips rendered inside the chat bubble (agent pane already renders them in the pane).}

## Notes

### Why `kind='bot'` instead of reusing `kind='webhook'`

Webhook bubbles intentionally suppress @mention/PM/profile because an inbound
webhook has no identity a member can address. Chat agents are addressable by
name — being @mentionable IS the trigger. Sharing the kind would force a flag to
branch every affordance; a clean second kind keeps both bubbles' rules local and
the webhook spec untouched. Both reuse the same denormalised `bot_name` /
`bot_avatar_url` columns, so the schema cost is one new `bot_agent_id` + one
`gen_status`.

### Why channel-history instead of a thread

The agent pane exists because someone wanted *deliberate, persistent* AI
conversations with their own history, separate from the chatter. In-channel, the
channel **is** the history — feeding the last ~30 messages makes the bot behave
like a member who scrolled up, which is exactly the "acts like a normal user"
intent. Threads would re-introduce the very separation this feature removes.
