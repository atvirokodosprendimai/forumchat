# spec - agent - per-community AI chat with threads and resumable streaming

status: implemented
created: 2026-06-20

## Intent

Each community gets an **Agent**: a ChatGPT-style AI chat with persistent
threads + history, reached at `/c/{slug}/agent`. The nav link sits directly
below **Chat**. The model is configured per community by an admin. First
provider is **Ollama** (local/self-hosted); the provider layer is an interface
so Claude / OpenAI drop in later, with credentials living in the community
admin section.

## Claims

- **Threads carry full context.** Every send replays the whole thread to the
  model (`Service.Send` → `Repo.Messages` → `buildHistory`), so a member just
  keeps typing to continue the conversation. Only *completed* assistant turns
  are fed back as context — a half-streamed or errored answer is skipped.
- **Two visibilities.** A thread is `private` (only its creator, like ChatGPT
  history) or `shared` (every approved member reads AND continues it). Chosen
  at creation; `ListThreads` returns "all shared + my private".
- **Share an answer to chat.** An assistant answer can be posted into any
  public chat channel as the requesting member (`PostShareToChannel` → the
  `ShareToChannel` closure wired to `chat.Service.Send`).
- **Backend-buffered streaming, 100ms fat-morph.** The generation runs in a
  detached server goroutine, not tied to the SSE request. Tokens accumulate in
  a buffer; a `time.Ticker(100ms)` flushes the buffer to the DB and broadcasts
  the thread id. Open SSE streams refetch + fat-morph `#agent-messages`. Brotli
  compresses the repeated full-conversation patch ~20×; the batched arrival
  reads as a smooth view-transition burst. (`runner.go`, `FlushInterval`.)
- **Resume across refresh/crash.** Because the DB is the single source of
  truth, a browser refresh / crash / tab-sleep reconnect renders whatever is
  saved and, since the goroutine keeps filling the DB, continues live. No
  client-held state.
- **Honest restart behaviour.** A *server* restart can't resume an LLM
  completion mid-stream, so on boot any lingering `generating` row is flipped
  to `interrupted` (`MarkGeneratingInterrupted`). The partial is kept and the
  bubble offers **Regenerate** (`Service.Regenerate` re-runs the last assistant
  turn from the prior context).
- **Stop.** A user can cancel an in-flight generation; the partial persists as
  `interrupted`.

## Boundaries (explicitly NOT)

- **No mid-completion server-restart resume.** Impossible with Ollama/Claude/
  OpenAI streaming APIs — we keep the partial and offer Regenerate instead.
- **No per-token wire frames.** The wire carries only the thread id ("changed")
  — every stream refetches from the DB and renders for its own viewer, exactly
  like chat's fat-morph (`internal/chat/CLAUDE.md §6`).
- **Admins do not read private threads.** Private = creator-only for read,
  write, and delete. Admins may delete *shared* threads.
- **No second writer.** One generation per thread at a time (`Runner.active`
  map); a concurrent send into a busy shared thread is refused, not queued.

## Design

| Layer | File |
|---|---|
| Schema | `migrations/00036_agent.sql` — `ai_configs`, `ai_threads`, `ai_messages` |
| Domain + errors | `internal/agent/agent.go` |
| Provider interface + Ollama NDJSON client | `internal/agent/provider.go` |
| SQL (config/threads/messages) | `internal/agent/repo.go` |
| Per-thread in-process fan-out | `internal/agent/bus.go` |
| Generation goroutine + 100ms flush | `internal/agent/runner.go` |
| Write orchestration + history build | `internal/agent/service.go` |
| HTTP boundary | `internal/agent/handler.go` |
| UI (two-pane, bubbles, composer, share modal, admin form) | `web/templ/agent.templ` |

Gated by `AI_ENABLED` (instance) **and** per-community `ai_configs.enabled`.
NATS subject `community.<cid>.agent.thread.<tid>`.

## Verification

`internal/agent/agent_test.go`: config round-trip, history-build + auto-title,
visibility filtering (private vs shared across two users), interrupt sweep,
Ollama streaming against a stub server, and a full runner end-to-end
(stub Ollama → 100ms flush → DB → `done`). Boot smoke confirms migration 00036
applies and routes mount with `AI_ENABLED=true`.

## Future

- Claude / OpenAI providers (add a `newProvider` branch + encrypt `api_key_enc`
  at rest using a boot key, like `SESSION_KEY`).
- Per-thread model picker ("user could choose model") — `ai_threads.model`
  column already exists.
- JetStream replay so cross-process reconnects see in-flight tokens without a
  full refetch.
