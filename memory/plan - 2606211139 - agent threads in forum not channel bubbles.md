---
name: plan-agent-forum-threads
status: completed
type: plan
spec: spec - chat-agents - in-channel-ai-participants-triggered-by-mention-or-prefix
tldr: Pivot chat-agents: a trigger no longer streams an in-channel bubble — it creates a FORUM THREAD (agent-owned), streams the agent's answer as a bot post, and announces the thread link back in chat. Every member's reply in that thread is a new prompt with the full thread history as context; the agent answers as the next post. Replaces the Phase-2 channel bubble.
---

# Plan — agent conversations as forum threads

Source: [[spec - chat-agents - in-channel-ai-participants-triggered-by-mention-or-prefix]]
(being updated — this supersedes its "live streaming bubble in the channel"
decision with "create a forum thread").

Branch: `task/agent-forum-threads`.

## Decisions (resolved with user 2606-06-21)

- **Surface = real forum thread + posts** (public `/forum`, reuses forum UI), NOT
  a shared `ai_threads` pane conversation.
- **Replace** the Phase-2 in-channel streaming bubble — every trigger now makes a
  thread. (The channel `chatagents.Runner` is removed.)
- **Any member** can drive the thread: each human reply is a new prompt; the
  agent's own posts never re-trigger (loop guard).
- **Streaming** the bot reply (post `gen_status` + 100ms flush, like chat) — default.
- **Schema: NO posts table rebuild.** `posts` carries FTS (00038) + RAG outbox
  (00039) triggers; rebuilding to null `author_id` would drop+recreate them.
  Instead keep `author_id` NOT NULL and author agent posts to a **sentinel bot
  user** (fixed id, status disabled), with real identity in new `agent_id` /
  `bot_name` / `bot_avatar_url` columns.

## Reuse map (Agile — extract, don't copy)

- Chat→forum bridge `forum.PostPromoteChat` (`handler.go:556`) = the template for
  "create thread + `thread_announce` back in chat" (`h.Chat.PostSystem` +
  `relayThreadAnnounce` + `ChatBus`).
- Streaming generation: reuse `agent.NewProvider` + the `FlushInterval` 100ms
  loop. If the forum runner loop ≈ `agent.Runner.run`, extract a shared core;
  else keep a slim forum runner (provider + ticker), no second copy of the
  channel runner (that one is deleted).
- Trigger detection: keep `chatagents` `match.go` + `dispatch.go`; repoint
  Dispatch from the channel runner to the new thread path.
- `forum` exposes a closure hook for replies (wired in main.go) to avoid a
  `forum ↔ chatagents` import cycle (chatagents imports forum, not vice-versa).

## Phase 1 — schema + forum bot-post identity (static)  `[no generation yet]`

1. [x] `migrations/00044_agent_forum_threads.sql`: threads +agent_id; posts
   +agent_id/bot_name/bot_avatar_url/gen_status (ADD COLUMN, triggers intact);
   sentinel bot user `agent-bot` (disabled) for agent posts' author_id FK.
2. [x] `internal/forum/forum.go`: `Thread.AgentID`; `Post.AgentID/BotName/BotAvatar/
   GenStatus` + `Post.IsBot()`; `CreateThread` insert + `CreateThreadInput.AgentID`;
   all 3 thread SELECTs + `scanThread` carry `agent_id`; `ListPosts`+`GetPost`
   scans carry the 4 post cols; agent_id set → AuthorName/Avatar from bot fields.
   (CreatePost unchanged — new cols use schema defaults.)
3. [x] `web/templ/forum.templ` `PostView` +IsBot/GenStatus/AuthorAvatar; `ForumPost`
   bot branch (🤖 + AI tag + `▍` cursor, mod-only delete, no quote/todo); mapped
   in `loadPostViews` (bot CanEdit = mod only). Reuses chat CSS tokens.
4. [x] Verify: `make gen && go build ./...` clean; `go test ./...` green; migration
   00044 applies + boot clean under AI_ENABLED, GET / → 200.

## Phase 2 — trigger → create thread + stream first reply; remove channel bubble

1. [x] `internal/forum`: `Repo.InsertBotPost`, `Repo.UpdateBotPostBody`,
   `Repo.MarkBotPostsInterrupted`, `AgentBotUserID` + gen consts;
   `forum.Handler.CreateAgentThread` (CreateThread w/ agent_id + thread_announce
   bridge — reuses `buildThreadAnnounce`/`deriveSubject`/`relayThreadAnnounce`).
2. [x] `internal/chatagents`: deleted channel `runner.go`+test; added `thread.go`
   `ThreadRunner` (imports forum+agent): provider + 100ms flush →
   `UpdateBotPostBody` + forum thread Bus/NATS broadcast; one gen per thread.
3. [x] `Dispatcher` repointed: `Trigger` struct + `CreateThreadFunc`; on a chat
   trigger → CreateThread closure → `ThreadRunner.Generate`. No channel bubble.
4. [x] `chat.Handler`: `AgentTrigger` struct + `Dispatch func(ctx, AgentTrigger)`;
   `PostSend` builds it. Removed dead `chat.Repo.UpdateBotBody`/
   `MarkBotGeneratingInterrupted`; swapped boot sweep → `forum.MarkBotPostsInterrupted`.
   `kind='bot'` chat columns/render left dormant; roster bot + mention stay.
5. [x] main.go: `NewThreadRunner(forumRepo,…)` + `NewDispatcher(agentRepo,
   forumHandler.CreateAgentThread, runner)`; `chatHandler.Dispatch` adapter.
6. [x] Test: `TestThreadRunnerStreamsReplyToDone` (stub-Ollama → thread bot post
   streams to done, identity + sentinel author asserts) + `MarkBotPostsInterrupted`.

## Phase 3 — reply-as-prompt (any member) + full thread history

1. [x] `forum.Handler.OnAgentReply func(ctx, communityID, threadID, agentID string)`
   (wired main.go → loads agent + `Generate`). `PostReply` fires it after a human
   post when `thread.AgentID` set. Loop guard: bot posts use `InsertBotPost`,
   never `PostReply`, so never re-trigger. (forum stays agent-free.)
2. [x] `ThreadRunner.buildHistory`: thread body = opening user turn; own bot posts
   → assistant; humans → user `name: body`; cap last `ContextLimit` posts; system
   = preamble + `system_prompt`.
3. [x] Reply path covered by the runner test (history includes posts) + loop guard
   by construction. (Phase 2+3 wiring merged — they share the runner.)
4. [x] Spec **Pivot** note + AGENTS.md §6.9 rewritten to the forum-thread model
   (§6.9.1 keeps the historical channel model). Full build+test green; boot clean.

## Verification (overall)

`make gen && go build ./... && go test ./...` green each phase; migration applies;
boot clean under `AI_ENABLED`. E2E: `@nick hi` in chat → thread_announce link →
open thread → answer streams → any member replies → answer streams as next post.

## Progress Log

- 2606211139 — plan from 3 locked decisions; branch `task/agent-forum-threads`. Starting Phase 1.
- 2606211145 — **Phase 1 done.** Schema 00044 (threads.agent_id; posts bot cols;
  sentinel agent-bot user) + forum bot-post identity + render. Build/test/boot green.
- 2606211153 — **Phase 2+3 done → plan COMPLETE.** Trigger now opens an agent forum
  thread (`CreateAgentThread` bridge) + streams the reply as a bot post
  (`ThreadRunner`); every human reply re-runs the agent over full thread history
  (`OnAgentReply`). Channel `chatagents.Runner` + dead chat-bot methods removed.
  Tests: thread-runner stub-Ollama end-to-end + sweep + matcher + binding — all
  green; `go build ./...` + `go test ./...` clean; boots clean under AI_ENABLED.
