package agent

import (
	"context"
	"encoding/json"
)

// RoleTool is the role of a tool-result turn fed back to the model.
const RoleTool = "tool"

// MaxToolIterations caps the agentic loop: model → tool calls → results →
// model → … Prevents a runaway tool-calling model from looping forever.
const MaxToolIterations = 8

// ToolDef advertises a callable tool to the provider. Schema is the JSON Schema
// (an "object") describing the tool's parameters, taken verbatim from the MCP
// server's inputSchema.
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolCall is one tool invocation the model requested. Args is the raw JSON
// arguments object the model produced.
type ToolCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ToolResult is a recorded tool execution, persisted on the assistant turn
// (ai_messages.tool_calls) so the chat shows which MCP tool the agent used and
// the trace survives refresh/replay. Result is display text (may be truncated).
type ToolResult struct {
	Server string `json:"server"` // which server answered ("internal" or an MCP server name)
	Name   string `json:"name"`
	Args   string `json:"args"`   // compact JSON, for the chip tooltip
	Result string `json:"result"` // text result, truncated for display
	OK     bool   `json:"ok"`
}

// ToolSet is the live union of tools for one generation: the internal search
// server plus the community's enabled MCP servers. Built per generation (so it
// is scoped to the agent's community) and closed when the generation ends.
type ToolSet interface {
	// Defs returns the advertised tools for the provider request.
	Defs() []ToolDef
	// Call executes a tool by name. It never returns an error to the caller —
	// a failure is surfaced as ok=false with the error text in `text`, so the
	// model receives it as a tool result and can recover. `server` names the
	// server that answered (for the chat trace).
	Call(ctx context.Context, name string, args json.RawMessage) (server, text string, ok bool)
	// Close releases all underlying MCP sessions/transports.
	Close()
}

// SearchHit is one result from the internal full-text search ("internal MCP").
type SearchHit struct {
	Kind      string // chat | thread | post
	RefID     string
	Title     string
	Snippet   string
	CreatedAt int64
}

// ToolResultDisplayMax bounds the text stored per tool result for display.
const ToolResultDisplayMax = 600

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func encodeToolCalls(t []ToolResult) string {
	if len(t) == 0 {
		return ""
	}
	b, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeToolCalls(s string) []ToolResult {
	if s == "" {
		return nil
	}
	var out []ToolResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
