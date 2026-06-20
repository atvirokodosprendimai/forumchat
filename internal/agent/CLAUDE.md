# agent

Per-community AI chat ("Agent") — ChatGPT-style threads + history at
`/c/{slug}/agent`, nav link below Chat. Gated by `AI_ENABLED` (instance) AND
having ≥1 enabled agent (`ai_agents.enabled`).

## Multi-agent (migration 00037)

A community defines several named **agents** (`ai_agents`), each a full
independent config: name, provider, base_url, model, api_key_enc, system_prompt,
`vision`, enabled. A thread pins to one agent (`ai_threads.agent_id`) for its
lifetime; the runner uses that agent's provider/model/system-prompt. Admin CRUD
lives at `/c/{slug}/admin/ai` (list + server-driven edit form, `agent.Handler`
GetAgents/GetNewAgentForm/GetEditAgentForm/PostSaveAgent/PostDeleteAgent). The
old singleton `ai_configs` was migrated into a default "Assistant" agent.

## Vision / image attach

`ai_agents.vision` → the composer shows an image attach (reuses `paste.js`
`fcPickImage`/`fcPasteImage` writing the `agent_image_data` hidden-input
signal). On send, the image is uploaded for display (small signed URL in
body_md — never a data: URL, which would bloat the 100ms morph) AND its raw
base64 is stored in `ai_messages.images` (JSON) and forwarded to the model
(`ChatMessage.Images`, Ollama's `/api/chat` shape). `buildHistory` replays the
images so regenerate/follow-ups keep them. Ollama accepts images only — PDF /
document understanding waits for the Claude/OpenAI providers.

## Tools / MCP (migration 00038)

A `tools_enabled` agent may call MCP tools while it answers ("Agent supports
tools" checkbox). The chat renders each call as a chip (persisted on
`ai_messages.tool_calls`).

- `tools.go` — `ToolDef` / `ToolCall` / `ToolResult` / `ToolSet` (the agent
  package depends ONLY on the `ToolSet` interface), `SearchHit`, tool-call
  JSON codec, `MaxToolIterations`.
- `mcp.go` — `ai_mcp_servers` repo + `MCPServer` domain + `Service.SaveMCPServer`
  (per-community external servers: stdio | http). Plus `Repo.SearchContent`
  (the FTS5 query behind the internal search tool).
- `internal/agent/mcpx/` — the ONLY package that imports the MCP SDK. `Manager.
  Build(ctx, agent)` assembles a per-generation `ToolSet`: a built-in in-process
  MCP server exposing `search` (community-scoped FTS) over an in-memory
  transport, plus the community's enabled external servers. Wired in `main.go`
  as `agentRunner.Tools` (closure pattern, like `ShareToChannel`).
  - **Adding an internal DB tool** (recipe — see `list_issues` / `get_issue`):
    1. add an optional `…Func` field on `mcpx.Manager` (community id is a
       PARAM, never a model arg — that scoping IS the authorization);
    2. register the tool in `internalSession` with `mcp.AddTool` inside an
       `if m.XFunc != nil {…}` guard (input struct uses `jsonschema:` tags);
    3. wire the closure in `main.go` with a community-scoped `WHERE
       p.community_id = ?` query (gate behind the relevant feature flag).
    Only expose community-public content — the generation runs detached and a
    shared thread has multiple readers, so don't surface per-user-private rows.
- The **agentic loop** lives in `runner.go` (`run`): model → tool calls →
  results → model, capped at `MaxToolIterations`. The provider only does one
  turn and reports tool calls; it never executes them. Ollama tool turns run
  `stream:false` (needs a tool-capable model); a tools-disabled agent and
  `SummarizeToThread` stream as before (pass `nil` tools).
- stdio external servers run arbitrary host commands → gated by
  `AGENT_MCP_ALLOW_STDIO` (default off). Internal search + http are unaffected.

## Shape

- `agent.go` — domain types, status/role/visibility consts, sentinel errors.
- `provider.go` — `Provider.Stream(ctx, model, msgs, tools, onDelta) →
  (*StreamResult, error)` + `Ollama` (direct NDJSON client to `/api/chat`).
  `newProvider(cfg)` selects by `cfg.Provider`; add Claude/OpenAI branches here.
- `repo.go` — all SQL (agent CRUD, threads + `agent_id`, messages + `images` +
  `tool_calls`, `SearchContent`, the boot `MarkGeneratingInterrupted` sweep).
- `bus.go` — per-thread in-process fan-out (copy of `lobbies.Bus`).
- `runner.go` — **the heart.** A detached goroutine streams the model; a
  `time.Ticker(FlushInterval=100ms)` writes the buffer to the DB and broadcasts
  the thread id. DB writes use a detached context so a Stop still persists the
  partial. `active map[threadID]cancel` enforces one generation per thread.
- `service.go` — write orchestration: `Send` (user turn + empty assistant
  placeholder + history), `Regenerate`, `buildHistory` (completed assistant
  turns only).
- `handler.go` — HTTP. Mirrors chat's fat-morph + SSE refetch loop.

## Why it resumes

The DB is the single source of truth. The runner fills it every 100ms; the SSE
stream just refetches + fat-morphs `#agent-messages`. So browser refresh/crash
resumes live for free. A *server* restart can't resume an LLM completion — the
boot sweep flips `generating → interrupted`, the partial is kept, the bubble
offers Regenerate.

## Gotchas

- The agent package must NOT import `chat`. Share-to-channel + channel listing
  come in as closures wired in `main.go` (`ShareToChannel`, `ListChannels`),
  same trick as `chat.ListProjects`.
- Wire payload is only the thread id ("changed") — never rendered HTML. See
  `internal/chat/CLAUDE.md §6.4`.
- Private threads are creator-only (read/write/delete). Admins may delete
  shared threads, but never read private ones.
- `api_key_enc` is reserved for hosted providers — Ollama needs none. Encrypt
  it at rest before shipping Claude/OpenAI.

<claude-mem-context>
# Recent Activity

<!-- This section is auto-generated by claude-mem. Edit content outside the tags. -->

*No recent activity*
</claude-mem-context>