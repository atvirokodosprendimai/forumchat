// Package mcpx wires the agent's tool support to MCP servers. It builds, per
// generation, the live ToolSet a tools-enabled agent uses: an in-process
// "internal" MCP server exposing full-text search over the community's content,
// plus any external MCP servers the community admin connected (stdio subprocess
// or streamable HTTP). The agent package depends only on its ToolSet interface;
// this package supplies the concrete implementation, wired in main.go.
package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SearchFunc runs the internal full-text search for a community. Wired to
// agent.Repo.SearchContent in main.go.
type SearchFunc func(ctx context.Context, communityID, query string, limit int) ([]agent.SearchHit, error)

// ServerConfig is one external MCP server a community connected.
type ServerConfig struct {
	Name      string
	Transport string // "stdio" | "http"
	Command   string
	Args      []string
	URL       string
	Headers   map[string]string
	Env       map[string]string
}

// ServersFunc returns a community's enabled external MCP servers. Wired in
// main.go to ai_mcp_servers.
type ServersFunc func(ctx context.Context, communityID string) []ServerConfig

// Manager builds tool sets. AllowStdio gates external stdio servers (they run
// arbitrary host commands) behind an instance-operator opt-in; HTTP servers are
// always allowed.
type Manager struct {
	Search     SearchFunc
	Servers    ServersFunc
	AllowStdio bool
	Log        *slog.Logger
}

// New builds a Manager.
func New(search SearchFunc, servers ServersFunc, allowStdio bool, log *slog.Logger) *Manager {
	return &Manager{Search: search, Servers: servers, AllowStdio: allowStdio, Log: log}
}

var clientImpl = &mcp.Implementation{Name: "forumchat", Version: "1"}

// Build assembles the ToolSet for agent a, scoped to its community. It connects
// the internal search server plus each enabled external server; a server that
// fails to connect is logged and skipped (a flaky MCP server must not break the
// whole turn). Returns (nil, nil) when no tools are usable.
func (m *Manager) Build(ctx context.Context, a agent.Agent) (agent.ToolSet, error) {
	ts := &toolSet{route: map[string]entry{}, log: m.Log}

	if m.Search != nil {
		if cs, srv, err := m.internalSession(ctx, a.CommunityID); err != nil {
			m.Log.Warn("mcpx: internal server", "err", err)
		} else {
			ts.sessions = append(ts.sessions, cs)
			ts.internalSrv = srv
			m.addTools(ctx, ts, cs, "internal")
		}
	}

	if m.Servers != nil {
		for _, cfg := range m.Servers(ctx, a.CommunityID) {
			if cfg.Transport == "stdio" && !m.AllowStdio {
				m.Log.Warn("mcpx: stdio server skipped (AGENT_MCP_ALLOW_STDIO is off)", "name", cfg.Name)
				continue
			}
			cs, err := m.externalSession(ctx, cfg)
			if err != nil {
				m.Log.Warn("mcpx: connect external server", "name", cfg.Name, "err", err)
				continue
			}
			ts.sessions = append(ts.sessions, cs)
			m.addTools(ctx, ts, cs, cfg.Name)
		}
	}

	if len(ts.defs) == 0 {
		ts.Close()
		return nil, nil
	}
	return ts, nil
}

// internalSession stands up an in-process MCP server with a community-scoped
// `search` tool and returns a client session connected to it over an in-memory
// transport. The community id is closed over, so this server can only ever
// search its own community's content.
func (m *Manager) internalSession(ctx context.Context, communityID string) (*mcp.ClientSession, *mcp.ServerSession, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "forumchat-internal", Version: "1"}, nil)

	search := m.Search
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "search",
		Description: "Full-text search this community's own chat messages and forum threads/posts. Returns ranked snippets with their source. Use it to ground answers in what members actually wrote here, rather than guessing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, any, error) {
		hits, err := search(ctx, communityID, in.Query, in.Limit)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: formatHits(hits)}}}, nil, nil
	})

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("connect internal server: %w", err)
	}
	cs, err := mcp.NewClient(clientImpl, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		ss.Close()
		return nil, nil, fmt.Errorf("connect internal client: %w", err)
	}
	return cs, ss, nil
}

// searchInput is the internal `search` tool's parameter schema (inferred by the
// SDK from these jsonschema tags).
type searchInput struct {
	Query string `json:"query" jsonschema:"the full-text search query (keywords)"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of results (default 10)"`
}

// externalSession connects to one configured MCP server.
func (m *Manager) externalSession(ctx context.Context, cfg ServerConfig) (*mcp.ClientSession, error) {
	var transport mcp.Transport
	switch cfg.Transport {
	case "http":
		if strings.TrimSpace(cfg.URL) == "" {
			return nil, fmt.Errorf("http server %q has no url", cfg.Name)
		}
		transport = &mcp.StreamableClientTransport{
			Endpoint:   cfg.URL,
			HTTPClient: &http.Client{Transport: headerRoundTripper{headers: cfg.Headers, base: http.DefaultTransport}},
		}
	case "stdio", "":
		if strings.TrimSpace(cfg.Command) == "" {
			return nil, fmt.Errorf("stdio server %q has no command", cfg.Name)
		}
		cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
		cmd.Env = append(os.Environ(), envSlice(cfg.Env)...)
		transport = &mcp.CommandTransport{Command: cmd}
	default:
		return nil, fmt.Errorf("unknown transport %q", cfg.Transport)
	}
	cs, err := mcp.NewClient(clientImpl, nil).Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect %q: %w", cfg.Name, err)
	}
	return cs, nil
}

// addTools lists a session's tools and registers them on the set (first server
// wins on a name collision, so the internal `search` is stable).
func (m *Manager) addTools(ctx context.Context, ts *toolSet, cs *mcp.ClientSession, server string) {
	lt, err := cs.ListTools(ctx, nil)
	if err != nil {
		m.Log.Warn("mcpx: list tools", "server", server, "err", err)
		return
	}
	for _, t := range lt.Tools {
		if _, dup := ts.route[t.Name]; dup {
			m.Log.Warn("mcpx: duplicate tool name skipped", "tool", t.Name, "server", server)
			continue
		}
		schema, err := json.Marshal(t.InputSchema)
		if err != nil || len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		ts.defs = append(ts.defs, agent.ToolDef{Name: t.Name, Description: t.Description, Schema: schema})
		ts.route[t.Name] = entry{session: cs, server: server}
	}
}

// --- the live tool set ----------------------------------------------------

type entry struct {
	session *mcp.ClientSession
	server  string
}

type toolSet struct {
	defs        []agent.ToolDef
	route       map[string]entry
	sessions    []*mcp.ClientSession
	internalSrv *mcp.ServerSession
	log         *slog.Logger
}

func (t *toolSet) Defs() []agent.ToolDef { return t.defs }

func (t *toolSet) Call(ctx context.Context, name string, args json.RawMessage) (server, text string, ok bool) {
	e, found := t.route[name]
	if !found {
		return "", "unknown tool: " + name, false
	}
	var argv any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argv); err != nil {
			return e.server, "invalid tool arguments: " + err.Error(), false
		}
	}
	res, err := e.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: argv})
	if err != nil {
		return e.server, "tool error: " + err.Error(), false
	}
	txt := contentText(res)
	if res.IsError {
		return e.server, txt, false
	}
	return e.server, txt, true
}

func (t *toolSet) Close() {
	for _, s := range t.sessions {
		_ = s.Close()
	}
	if t.internalSrv != nil {
		_ = t.internalSrv.Close()
	}
}

// --- helpers --------------------------------------------------------------

func formatHits(hits []agent.SearchHit) string {
	if len(hits) == 0 {
		return "No matches found."
	}
	var b strings.Builder
	for i, h := range hits {
		title := strings.TrimSpace(h.Title)
		if title == "" {
			title = h.Kind
		}
		fmt.Fprintf(&b, "%d. [%s] %s\n   %s\n", i+1, h.Kind, title, strings.TrimSpace(h.Snippet))
	}
	return strings.TrimSpace(b.String())
}

func contentText(res *mcp.CallToolResult) string {
	parts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
			continue
		}
		if b, err := c.MarshalJSON(); err == nil {
			parts = append(parts, string(b))
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			return string(b)
		}
	}
	return strings.Join(parts, "\n")
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// headerRoundTripper injects static headers (e.g. Authorization) on every
// request to an HTTP MCP server.
type headerRoundTripper struct {
	headers map[string]string
	base    http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if len(h.headers) > 0 {
		req = req.Clone(req.Context())
		for k, v := range h.headers {
			req.Header.Set(k, v)
		}
	}
	return h.base.RoundTrip(req)
}
