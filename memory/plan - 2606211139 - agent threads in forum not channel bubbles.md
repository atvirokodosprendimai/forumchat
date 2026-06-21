---
name: plan-agent-forum-threads
status: active
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

1. [ ] `migrations/00044_agent_forum_threads.sql`:
   - `threads` ADD `agent_id TEXT REFERENCES ai_agents(id) ON DELETE SET NULL`
     (marks an agent thread; thread is authored by the triggering human).
   - `posts` ADD `agent_id TEXT REFERENCES ai_agents(id) ON DELETE SET NULL`,
     `bot_name TEXT NOT NULL DEFAULT ''`, `bot_avatar_url TEXT NOT NULL DEFAULT ''`,
     `gen_status TEXT NOT NULL DEFAULT ''`. (simple ADD COLUMN — triggers intact.)
   - INSERT sentinel bot user (fixed id `agent-bot`, status disabled) for agent
     posts' `author_id` FK.
2. [ ] `internal/forum/forum.go`: `Thread.AgentID *string`; `Post.AgentID *string`
   + `BotName/BotAvatar/GenStatus`; `CreateThread` insert carries `agent_id`;
   post insert + `ListPosts`/`PostByID` scans carry the 4 cols; when `agent_id`
   set, populate `AuthorName/AuthorAvatar` from bot fields.
3. [ ] `web/templ/forum.templ` `PostView`: `IsBot` + `GenStatus`; bot post renders
   🤖 + name + body + `▍` cursor while generating; suppress author affordances
   (no quote/edit-by-author). Mirror chat's bubble (reuse `MsgKindBot` styling
   tokens). Map in `loadPostViews`.
4. [ ] Verify: `make gen && go build ./... && go test ./...`; migration applies +
   boot clean; hand-insert a bot post → renders.

## Phase 2 — trigger → create thread + stream first reply; remove channel bubble

1. [ ] `internal/forum`: `Repo.InsertBotPost`, `Repo.UpdateBotPostBody`,
   `Repo.MarkBotPostsInterrupted`; `Service`/closure to create an agent thread
   (subject from trigger first line, body = prompt, `agent_id` set).
2. [ ] `internal/chatagents`: delete the channel `runner.go`; add `thread.go`
   `ThreadRunner` (imports forum+agent): create thread (or reuse a thread-create
   closure), run `agent.NewProvider` + 100ms flush → `forum.Repo.UpdateBotPostBody`
   + forum thread Bus broadcast; one generation per (thread) `active` map.
3. [ ] `Dispatcher.Dispatch` repointed: on a chat trigger, create the agent
   thread, kick `ThreadRunner`, and announce the thread link in chat (reuse the
   `thread_announce` bridge via a closure — no channel bot bubble).
4. [ ] Remove the Phase-2 channel wiring: `chatHandler.Dispatch` now routes to the
   thread path; `chat.Repo.UpdateBotBody` / `MarkBotGeneratingInterrupted` and the
   `kind='bot'` channel-message render become unused (keep columns; drop the
   channel runner + its boot sweep). Roster bot + mention stay (still the trigger).
5. [ ] Boot sweep `forum.Repo.MarkBotPostsInterrupted`; main.go rewiring.
6. [ ] Tests: thread-runner stub-Ollama → thread created + bot post streams to done.

## Phase 3 — reply-as-prompt (any member) + full thread history

1. [ ] `forum.Handler` gets `OnAgentReply func(ctx, threadID, communityID string)`
   closure (wired main.go → chatagents). `PostReply`: after a human post lands in
   a thread whose `agent_id` is set, fire it (detached). Loop guard: a post with
   `agent_id` set (the bot's own) never fires.
2. [ ] `ThreadRunner` reply path: `buildHistory` from the thread (body + all posts
   oldest→newest; bot posts → assistant, humans → user `name: body`; system =
   preamble + `system_prompt`), stream a new bot post.
3. [ ] Tests: a human reply triggers a bot post; a bot post does not re-trigger.
4. [ ] Update spec (status, decisions, render section) + AGENTS.md §6.9 to the
   forum-thread model. Verify full build+test green; boot smoke.

## Verification (overall)

`make gen && go build ./... && go test ./...` green each phase; migration applies;
boot clean under `AI_ENABLED`. E2E: `@nick hi` in chat → thread_announce link →
open thread → answer streams → any member replies → answer streams as next post.

## Progress Log

- 2606211139 — plan from 3 locked decisions; branch `task/agent-forum-threads`. Starting Phase 1.
