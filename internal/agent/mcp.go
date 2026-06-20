package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// MCP transports.
const (
	MCPTransportStdio = "stdio"
	MCPTransportHTTP  = "http"
)

// MaxMCPServersPerCommunity caps connected external MCP servers per community.
const MaxMCPServersPerCommunity = 20

var (
	ErrMCPName      = errors.New("agent: MCP server name is required")
	ErrMCPTransport = errors.New("agent: MCP transport must be stdio or http")
	ErrMCPTarget    = errors.New("agent: stdio needs a command, http needs a url")
	ErrMCPCap       = errors.New("agent: too many MCP servers")
)

// MCPServer is one external MCP server a community connected. Args/Headers/Env
// are stored as JSON. The internal full-text search server is built-in and is
// NOT represented here.
type MCPServer struct {
	ID          string
	CommunityID string
	Name        string
	Transport   string // stdio | http
	Command     string
	Args        []string
	URL         string
	Headers     map[string]string
	Env         map[string]string
	Enabled     bool
	Position    int
	UpdatedBy   string
	CreatedAt   int64
	UpdatedAt   int64
}

const mcpCols = `id, community_id, name, transport, command, args, url, headers, env,
	enabled, position, COALESCE(updated_by,''), created_at, updated_at`

func scanMCP(s interface {
	Scan(dest ...any) error
}) (MCPServer, error) {
	var m MCPServer
	var args, headers, env string
	var enabled int
	err := s.Scan(&m.ID, &m.CommunityID, &m.Name, &m.Transport, &m.Command, &args, &m.URL,
		&headers, &env, &enabled, &m.Position, &m.UpdatedBy, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return MCPServer{}, err
	}
	m.Args = decodeStrSlice(args)
	m.Headers = decodeStrMap(headers)
	m.Env = decodeStrMap(env)
	m.Enabled = enabled != 0
	return m, nil
}

// ListMCPServers returns a community's MCP servers in display order (admin list).
func (r *Repo) ListMCPServers(ctx context.Context, communityID string) ([]MCPServer, error) {
	return r.queryMCP(ctx, `SELECT `+mcpCols+` FROM ai_mcp_servers
		WHERE community_id = ? ORDER BY position, name`, communityID)
}

// ListEnabledMCPServers returns only the enabled servers (runtime tool wiring).
func (r *Repo) ListEnabledMCPServers(ctx context.Context, communityID string) ([]MCPServer, error) {
	return r.queryMCP(ctx, `SELECT `+mcpCols+` FROM ai_mcp_servers
		WHERE community_id = ? AND enabled = 1 ORDER BY position, name`, communityID)
}

func (r *Repo) queryMCP(ctx context.Context, q string, args ...any) ([]MCPServer, error) {
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers: %w", err)
	}
	defer rows.Close()
	var out []MCPServer
	for rows.Next() {
		m, err := scanMCP(rows)
		if err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MCPServerByID loads one server. Returns ErrNotFound when absent.
func (r *Repo) MCPServerByID(ctx context.Context, id string) (MCPServer, error) {
	m, err := scanMCP(r.DB.QueryRowContext(ctx, `SELECT `+mcpCols+` FROM ai_mcp_servers WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MCPServer{}, ErrNotFound
	}
	if err != nil {
		return MCPServer{}, fmt.Errorf("mcp server by id: %w", err)
	}
	return m, nil
}

// CountMCPServers returns how many MCP servers a community has.
func (r *Repo) CountMCPServers(ctx context.Context, communityID string) (int, error) {
	var n int
	err := r.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM ai_mcp_servers WHERE community_id = ?`, communityID).Scan(&n)
	return n, err
}

// CreateMCPServer inserts a server.
func (r *Repo) CreateMCPServer(ctx context.Context, m MCPServer) error {
	var updatedBy any
	if m.UpdatedBy != "" {
		updatedBy = m.UpdatedBy
	}
	_, err := r.DB.ExecContext(ctx, `
		INSERT INTO ai_mcp_servers (id, community_id, name, transport, command, args, url, headers, env,
			enabled, position, created_at, updated_at, updated_by)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.CommunityID, m.Name, m.Transport, m.Command, encodeStrSlice(m.Args), m.URL,
		encodeStrMap(m.Headers), encodeStrMap(m.Env), boolToInt(m.Enabled), m.Position,
		m.CreatedAt, m.UpdatedAt, updatedBy)
	if err != nil {
		return fmt.Errorf("create mcp server: %w", err)
	}
	return nil
}

// SetMCPServerEnabled flips a server on/off.
func (r *Repo) SetMCPServerEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := r.DB.ExecContext(ctx, `UPDATE ai_mcp_servers SET enabled = ?, updated_at = ? WHERE id = ?`,
		boolToInt(enabled), nowUnix(), id)
	return err
}

// DeleteMCPServer removes a server.
func (r *Repo) DeleteMCPServer(ctx context.Context, id string) error {
	_, err := r.DB.ExecContext(ctx, `DELETE FROM ai_mcp_servers WHERE id = ?`, id)
	return err
}

// SaveMCPServer validates and inserts a new external MCP server (Service write
// side; mirrors SaveAgent). Edits are delete-and-re-add for now.
func (s *Service) SaveMCPServer(ctx context.Context, m MCPServer) (MCPServer, error) {
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		return MCPServer{}, ErrMCPName
	}
	m.Transport = strings.TrimSpace(m.Transport)
	if m.Transport == "" {
		m.Transport = MCPTransportStdio
	}
	if m.Transport != MCPTransportStdio && m.Transport != MCPTransportHTTP {
		return MCPServer{}, ErrMCPTransport
	}
	m.Command = strings.TrimSpace(m.Command)
	m.URL = strings.TrimSpace(m.URL)
	switch m.Transport {
	case MCPTransportStdio:
		if m.Command == "" {
			return MCPServer{}, ErrMCPTarget
		}
	case MCPTransportHTTP:
		if m.URL == "" {
			return MCPServer{}, ErrMCPTarget
		}
	}
	n, err := s.Repo.CountMCPServers(ctx, m.CommunityID)
	if err != nil {
		return MCPServer{}, err
	}
	if n >= MaxMCPServersPerCommunity {
		return MCPServer{}, ErrMCPCap
	}
	now := nowUnix()
	m.ID = uuid.NewString()
	m.Position = n
	m.CreatedAt = now
	m.UpdatedAt = now
	if err := s.Repo.CreateMCPServer(ctx, m); err != nil {
		return MCPServer{}, err
	}
	return m, nil
}

// --- JSON column helpers --------------------------------------------------

func encodeStrSlice(v []string) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeStrSlice(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func encodeStrMap(v map[string]string) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeStrMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}
