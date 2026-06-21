---
name: plan-chat-agents
status: active
type: plan
spec: spec - chat-agents - in-channel-ai-participants-triggered-by-mention-or-prefix
tldr: Make per-community ai_agents first-class chat participants — roster bot icon, @mentionable, triggered per-agent by mention/prefix/both/all, scoped to admin-assigned channels, streaming kind='bot' bubble built from last ~30 channel messages. Reuses the agent runner/provider + webhook bot-identity columns. Gated by AI_ENABLED + per-agent in_chat_enabled.
---

# Plan — chat-agents (in-channel AI participants)

Source of truth: [[spec - chat-agents - in-channel-ai-participants-triggered-by-mention-or-prefix]].
Reuses: [[spec - agent - per-community-ai-chat-with-threads-and-resumable-streaming]] (runner/provider/ai_agents),
[[spec - webhooks - inbound-bot-messages-and-outbound-event-relay]] (bot-identity columns, kind machinery),
[[spec - chat-channels - admin-curated-public-text-channels]] (per-channel binding).

Branch: `task/spec-chat-agents` (spec already committed here, 0a6088c).

## Phase 1 — schema + kind='bot' identity + roster + mention union  `[static, no generation]`

Goal: a bot agent shows in the roster with a 🤖 icon and a `kind='bot'`
message renders correctly, **before** any generation exists. Seed a test agent
via SQL (`in_chat_enabled=1` + an `ai_agent_channels` row) until the admin form
lands in Phase 3.

1. [ ] `migrations/00043_chat_agents.sql`:
   - ALTER `ai_agents` ADD `in_chat_enabled INTEGER NOT NULL DEFAULT 0`,
     `trigger_mode TEXT NOT NULL DEFAULT 'mention'`
     `CHECK (trigger_mode IN ('mention','prefix','both','all'))`,
     `trigger_prefix TEXT NOT NULL DEFAULT '.'`, `avatar_url TEXT NOT NULL DEFAULT ''`.
   - CREATE TABLE `ai_agent_channels (agent_id, channel_id, PK(agent_id,channel_id))`
     both FK `ON DELETE CASCADE`.
   - ALTER `chat_messages` ADD `bot_agent_id TEXT REFERENCES ai_agents(id) ON DELETE SET NULL`,
     `gen_status TEXT NOT NULL DEFAULT ''`.
   - Down: reverse (SQLite drop-column / drop-table).
2. [ ] `internal/chat/chat.go`:
   - `KindBot Kind = "bot"`.
   - `Message.BotAgentID *string`, `Message.GenStatus string`.
   - INSERT column lists (`Insert`, `InsertWithAttachments`) carry `bot_agent_id`, `gen_status`.
   - `Recent`, `listBefore`, `ByID` SELECT + scan carry the 2 cols; `kind='bot'`
     populates `AuthorName`/`AuthorAvatar` from `bot_name`/`bot_avatar_url` (same
     branch as `KindWebhook`).
3. [ ] `web/templ/chat.templ`: `MsgKindBot`; bubble like a user bubble (avatar+name)
   with a 🤖 badge, **is** an @mention target, **no** PM/profile/reply-to-author,
   mods can delete; show a `▍` cursor while `GenStatus == "generating"`.
4. [ ] `internal/agent/repo.go` (or new read helpers): `ListInChatAgents(ctx, communityID)`
   and `ChannelsForAgent` / `AgentsForChannel(ctx, communityID, channelID)` over
   `ai_agents` ⨝ `ai_agent_channels` (enabled + in_chat_enabled only).
5. [ ] `internal/presence/handler.go` + `web/templ/roster.templ`:
   `RosterMember.IsBot bool`; the roster `push` appends one synthetic
   always-online entry per in-chat agent bound to the viewer's active channel
   (needs the channel id in the roster stream — confirm it's available, else add).
   Render the 🤖 icon; no profile/PM menu for bot rows.
6. [ ] `internal/chat/handler.go` `/chat/mention`: union member results with the
   in-chat agent names bound to the channel.
7. [ ] Verify: `make gen && make build && make test`. Manual: seed an agent
   (`in_chat_enabled=1`, bound to `#general`) via SQL, load chat → 🤖 in roster,
   `@n…` autocompletes it, hand-insert a `kind='bot'` row → bubble renders.

## Phase 2 — trigger matcher + dispatch + streaming generation + loop guard

Goal: `@nick hi` (or `.nick hi`) in a bound channel makes nick stream a reply.

1. [ ] `internal/chatagents/match.go`: pure `Match(agent, body) bool` honoring
   `trigger_mode` (mention word-boundary / prefix lone-vs-`<prefix><name>` /
   both / all). Table-tested.
2. [ ] `internal/chatagents/dispatch.go`: `Dispatch(ctx, communityID, channelID, msg)`
   — **no-op unless `msg.Kind == KindUser`** (loop guard); load agents bound to
   the channel; for each match, kick a generation. One gen per `(agent, channel)`
   via the runner `active` map key `chat:<channelID>:<agentID>` (drop if busy).
3. [ ] `internal/chatagents/runner.go`: `buildChannelHistory` (last
   `ChatAgentContextLimit`≈30 non-bot msgs via `chat.Repo.Recent`; bot's own
   `bot_agent_id` msgs → assistant, others → user prefixed w/ display name;
   system = `agent.system_prompt` + name preamble). Insert placeholder
   `kind='bot'` row (`gen_status='generating'`), reuse `internal/agent` provider
   + 100ms flush loop to rewrite `body_md` + broadcast channel id; `done` /
   `interrupted` terminal states; Regenerate path.
4. [ ] Wire `Dispatch` into `chat.Handler.PostSend` **after** the user message's
   fan-out. Boot: extend the agent interrupt sweep to flip stranded `kind='bot'`
   `gen_status='generating'` rows → `interrupted`.
5. [ ] `cmd/app/main.go`: build the `chatagents` orchestrator when `AI_ENABLED`;
   inject `chat.Service`/`Repo`/`Bus`, `agent` provider factory, NATS.
6. [ ] Tests: `match_test.go` (matrix incl. loop-guard); `runner_test.go` with a
   stub Ollama (mirror `agent_test.go`) → placeholder row → flush rewrites →
   `done`; interrupt sweep flips a bot gen row.
7. [ ] Verify: `AI_ENABLED=true`, seeded agent → `@nick hi` streams; `.nick hi`
   same; plain message → silent; an inbound webhook post → silent (loop guard).

## Phase 3 — admin form + cache invalidation

Goal: an admin toggles an agent into chat from the existing AI admin editor.

1. [ ] `web/templ/agent.templ` admin section: **Participate in chat** checkbox
   (`in_chat_enabled`), **Avatar URL**, **Trigger** select (mention|prefix|both|all)
   + **prefix** input (shown when mode≠mention), **Channels** multi-select.
2. [ ] `internal/agent` handler/service: persist the new `ai_agents` fields +
   replace `ai_agent_channels` bindings in one tx.
3. [ ] Invalidate the per-community roster/autocomplete agent cache + fire
   `presence.Bump(communityID)` on any of these mutations so open chat tabs
   re-render live.
4. [ ] README env note (none new — reuse `AI_ENABLED`) + AGENTS.md `§` on
   chat-agents (kind='bot', loop guard, seam package). Update CLAUDE.md if a new
   gotcha surfaces.
5. [ ] Verify: admin binds agent `nick` to `#general` with `trigger_mode=both`;
   end-to-end from a fresh tab with no SQL seeding.

## Decisions (resolved with user 2026-06-21, see spec)

- **Trigger:** per-agent configurable `mention | prefix | both | all`, `trigger_prefix` default `.`.
- **Channel scope:** admin-assigned via `ai_agent_channels`.
- **Delivery:** live streaming `kind='bot'` bubble (reuse agent runner).
- **Context:** last ~30 non-bot channel messages.
- **Identity:** NEW `kind='bot'` (mentionable + 🤖), NOT reused `kind='webhook'`.
- **Seam:** new pkg `internal/chatagents` (imports both `chat` + `agent`, avoids cycle).
- **Safety:** loop guard — only `kind='user'` triggers; bot-to-bot disabled v1.
- **Flag:** reuse `AI_ENABLED` + per-agent `in_chat_enabled`; no new instance flag.

## Verification (overall)

`make gen && make build && make test` green at the end of every phase. Spec
Verification section is the acceptance checklist (matcher table, dispatch scope,
runner stub-Ollama, identity/render, roster, E2E smoke).

## Progress Log

- 2606211058 — plan created from spec; branch `task/spec-chat-agents`. Starting Phase 1.
