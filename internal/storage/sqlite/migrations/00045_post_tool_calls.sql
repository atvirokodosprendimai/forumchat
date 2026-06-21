-- +goose Up

-- An agent reply post can carry a tool-call trace (the agentic loop's
-- internal-search / MCP-server calls), JSON-encoded like ai_messages.tool_calls,
-- so the forum bot post renders the same 🔧 chips as the agent pane.
ALTER TABLE posts ADD COLUMN tool_calls TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE posts DROP COLUMN tool_calls;
