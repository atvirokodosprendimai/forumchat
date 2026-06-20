// Package agent implements the per-community AI assistant ("Agent"): a
// ChatGPT-style chat with persistent threads + history backed by SQLite. A
// community can define several named agents, each a full independent config
// (provider, connection, model, key, system prompt) with an optional vision
// flag. A thread pins to one agent for its lifetime. A pluggable provider
// (Ollama first; Claude/OpenAI later) streams the answer into the DB on a
// 100ms cadence so any open SSE stream fat-morphs the whole conversation.
package agent

import "errors"

// Role of a message turn.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// Assistant message lifecycle.
const (
	StatusGenerating  = "generating"  // streaming in progress
	StatusDone        = "done"        // completed normally
	StatusInterrupted = "interrupted" // stopped by user, or orphaned by a restart (partial kept)
	StatusError       = "error"       // provider failed
)

// Thread visibility.
const (
	VisibilityPrivate = "private" // only the creator
	VisibilityShared  = "shared"  // every approved member can read + continue
)

// Provider ids.
const (
	ProviderOllama = "ollama"
)

// MaxAgentsPerCommunity is a soft cap on named agents per community.
const MaxAgentsPerCommunity = 20

var (
	ErrEmptyBody   = errors.New("agent: empty message")
	ErrNotFound    = errors.New("agent: not found")
	ErrForbidden   = errors.New("agent: forbidden")
	ErrDisabled    = errors.New("agent: no AI agent is available for this community")
	ErrGenerating  = errors.New("agent: a reply is already being generated")
	ErrBadProvider = errors.New("agent: unknown provider")
	ErrNoName      = errors.New("agent: name is required")
	ErrAgentCap    = errors.New("agent: too many agents")
)

// Agent is one named AI persona in a community (one row in ai_agents). Each
// carries its own full provider config so one agent can be a local Ollama and
// another a hosted model.
type Agent struct {
	ID           string
	CommunityID  string
	Name         string
	Provider     string
	BaseURL      string
	Model        string
	APIKeyEnc    string
	SystemPrompt string
	Vision       bool // model accepts image input → composer offers attach
	Enabled      bool
	Position     int
	UpdatedBy    string
	CreatedAt    int64
	UpdatedAt    int64
}

// Thread is one conversation, pinned to a single agent.
type Thread struct {
	ID          string
	CommunityID string
	UserID      string // creator
	AgentID     string
	Visibility  string
	Title       string
	Model       string
	CreatedAt   int64
	UpdatedAt   int64
}

// Message is one turn in a thread. Images are base64-encoded image payloads
// attached to a user turn (vision agents only); they ride to the model but are
// never rendered from here (the bubble shows an uploaded, signed image URL in
// body_html instead, to keep the fat-morph small).
type Message struct {
	ID        string
	ThreadID  string
	Role      string
	AuthorID  string // member who typed a user turn; "" for assistant/system
	BodyMD    string
	BodyHTML  string
	Status    string
	Error     string
	Images    []string
	CreatedAt int64
	UpdatedAt int64
}
