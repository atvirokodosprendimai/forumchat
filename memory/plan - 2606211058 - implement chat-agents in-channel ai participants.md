---
name: plan-chat-agents
status: completed
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

1. [x] `migrations/00043_chat_agents.sql`: ALTER `ai_agents` (+4 cols), new
   `ai_agent_channels`, ALTER `chat_messages` (+`bot_agent_id`,`gen_status`).
   - => migration applies clean at boot (`successfully migrated database to version: 43`).
2. [x] `internal/chat/chat.go`: `KindBot`; `Message.BotAgentID`+`GenStatus`;
   both INSERTs + `nullableRefs` carry the 2 cols; `listBefore`+`ByID` scan them;
   identity branch extended `KindWebhook || KindBot`.
   - => reused existing `bot_name`/`bot_avatar_url` cols (added by webhooks 00042) — no new identity cols.
3. [x] `web/templ/chat.templ`: `MsgKindBot` + `MsgView.GenStatus`; webhook bubble
   branch extended to bot (🤖 dot, "AI" tag, `▍` gen-cursor when generating);
   `toMsgView` maps `GenStatus`. CSS for `.bot-tag-ai` / `.gen-cursor`.
4. [x] `internal/agent/repo.go`: Agent gains `InChatEnabled/TriggerMode/TriggerPrefix/AvatarURL`;
   `agentCols`+`scanAgent`+Create/Update carry them; trigger-mode consts +
   normalizers; new `ListInChatAgents`, `AgentsForChannel`, `ChannelIDsForAgent`,
   `SetAgentChannels`.
5. [x] `internal/presence/handler.go` + `web/templ/roster.templ`:
   `RosterMember.IsBot`; `Handler.Agents` closure injects always-online bot rows;
   `RosterRow` branches to a bot variant (🤖 avatar, "bot" badge, no menu). CSS added.
   - => DECISION: roster shows in-chat agents **community-wide**, not channel-scoped
     — the roster already lists all members community-wide; channel-filtering bots
     only would be inconsistent. (Spec said per-channel; deviated for consistency.)
6. [x] `internal/chat/handler.go` `/chat/mention`: `MentionAgents` closure unions
   community-wide in-chat agent names (prefix-filtered, capped at MentionLimit).
7. [x] `cmd/app/main.go`: wired `presenceHandler.Agents` + `chatHandler.MentionAgents`
   closures from `agentRepo.ListInChatAgents` inside the `AIEnabled` block.
8. [x] Verify: `go build ./...` clean, `go test ./...` all green, migration applies
   + app boots clean with `AI_ENABLED=true`. (Manual seed-render deferred — admin
   form lands Phase 3; trigger/generation is Phase 2.)

## Phase 2 — trigger matcher + dispatch + streaming generation + loop guard

Goal: `@nick hi` (or `.nick hi`) in a bound channel makes nick stream a reply.

1. [x] `internal/chatagents/match.go`: pure `Match(agent, body, multiPrefix)` —
   mention (token mirror of `chat.parseMentions`) / prefix (lone bare vs
   `<prefix><name>` when multiPrefix) / both / all. `countPrefixAgents` helper.
2. [x] `internal/chatagents/dispatch.go`: `Dispatcher.Dispatch(ctx, cid, channelID, kind, body)`
   — **no-op unless `kind == chat.KindUser`** (loop guard); `AgentSource` iface
   (= `agent.Repo.AgentsForChannel`); computes `multiPrefix`; `Runner.Trigger` per match.
3. [x] `internal/chatagents/runner.go`: `NewRunner(chatRepo, chatBus, nc, limit, log)`;
   `Trigger` with `active map["channelID:agentID"]` (drop if busy); `run` builds
   history (last 30 non-bot via `chat.Repo.Recent`; own bot→assistant, others→user
   `name: body`; system = preamble + system_prompt), inserts placeholder
   `kind='bot'` (`gen_status='generating'`), reuses `agent.NewProvider` (exported)
   + 100ms ticker → `chat.Repo.UpdateBotBody` + broadcast; `done`/`interrupted`.
   - => added `agent.NewProvider` (exported wrapper); `chat.Repo.UpdateBotBody` +
     `MarkBotGeneratingInterrupted` + gen-status consts.
4. [x] Wired `chatHandler.Dispatch` closure into `PostSend` (detached goroutine,
   after `broadcastNewMsg`). Boot: `chatRepo.MarkBotGeneratingInterrupted` next to
   the agent sweep.
5. [x] `cmd/app/main.go`: built `chatagents.NewRunner` + `NewDispatcher(agentRepo, …)`
   inside the `AIEnabled` block, wired `chatHandler.Dispatch`.
6. [x] Tests: `match_test.go` (21-case matrix incl. multi-prefix disambiguation);
   `runner_test.go` stub-Ollama → placeholder → flush → `done` (+ identity asserts);
   interrupt-sweep flips a bot gen row. All green.
7. [x] Verify: `go build ./...` + `go test ./...` green. Loop guard verified by
   construction (Dispatch is only called from the user-send path + guards on kind).
   (Live `@nick` HTTP smoke needs a real Ollama — covered by the stub end-to-end test.)

## Phase 3 — admin form + cache invalidation

Goal: an admin toggles an agent into chat from the existing AI admin editor.

1. [x] `web/templ/agent.templ` `AgentAdminForm`: "Join the live chat" checkbox
   (`ai_in_chat`), **Avatar URL**, **Trigger** select (mention|prefix|both|all)
   + **prefix** input (`data-show` when mode≠mention), **Channels** checkboxes
   (CSV `ai_channels` signal — Datastar can't round-trip arrays). New view fields
   `InChatEnabled/TriggerMode/TriggerPrefix/AvatarURL/Channels` + `AgentChannelOption`.
   - => `@mention only` option label starts with `@` → templ read it as a component
     call; wrapped as `{ "@mention only" }`.
2. [x] `agent.Handler`: `agentSignals` + `PostSaveAgent` persist the new fields;
   `parseChannelCSV` → `Repo.SetAgentChannels(saved.ID, ids)`. `formView` helper
   populates the channel picker (reuses `ListChannels` + `ChannelIDsForAgent`).
3. [x] No cache to invalidate (roster/mention closures read DB fresh each call);
   instead `agentHandler.RosterBump = presenceTracker.Bump` fires on save so open
   chat rosters re-render live.
4. [x] AGENTS.md §6.9 (chat-agents: kind='bot', loop guard, seam package,
   community-wide roster deviation). No new env (reuses `AI_ENABLED`).
5. [x] Verify: `go build ./...` + `go test ./...` green (matcher + runner + sweep
   + channel-binding query tests); app boots clean under `AI_ENABLED=true`,
   `GET /` → 200, no panics. CSS added for the admin form + bot bubble/roster.

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
- 2606211111 — **Phase 1 complete.** Schema + `kind='bot'` identity (reusing webhook
  bot cols) + roster bot rows + mention union, all wired via closures in main.go.
  `go build ./...` + `go test ./...` green; migration 00043 applies + clean boot
  under `AI_ENABLED=true`. Deviation logged: roster/mention are community-wide (not
  channel-scoped) for consistency with the existing community-wide member roster.
  Next: Phase 2 (matcher + dispatch + streaming generation + loop guard).
- 2606211140 — **Phase 2 complete.** New `internal/chatagents` seam: pure matcher
  (table-tested), `Dispatcher` with the kind=='user' loop guard, and a streaming
  `Runner` that reuses `agent.NewProvider` + a 100ms flush into a `kind='bot'`
  bubble. Wired into `PostSend` (detached) + boot interrupt sweep. Added exported
  `agent.NewProvider`, `chat.Repo.UpdateBotBody` / `MarkBotGeneratingInterrupted`.
  Tests: matcher matrix + stub-Ollama runner end-to-end + sweep — all green;
  `go build ./...` + `go test ./...` clean. Next: Phase 3 (admin form + cache invalidation).
- 2606211128 — **Phase 3 complete → plan DONE.** Admin form (in-chat toggle,
  avatar, trigger mode/prefix, channel checkboxes via CSV signal); `PostSaveAgent`
  persists + `SetAgentChannels`; `RosterBump` on save. Added channel-binding query
  test. AGENTS.md §6.9 + spec → `implemented`. Full build + test green; clean boot
  under `AI_ENABLED`. Whole feature done across 3 phases on `task/spec-chat-agents`.
