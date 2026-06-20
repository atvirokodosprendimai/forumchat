// Package agent implements the per-community AI assistant ("Agent"): a
// ChatGPT-style chat with persistent threads + history backed by SQLite, a
// pluggable model provider (Ollama first; Claude/OpenAI later), and a
// generation runner that streams the model's answer into the DB on a 100ms
// cadence so any open SSE stream fat-morphs the whole conversation. Because
// the DB is the single source of truth, a page refresh or browser crash
// resumes a live generation for free; a server restart shows the persisted
// partial marked interrupted with a Regenerate affordance.
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

// Provider defaults.
const (
	ProviderOllama = "ollama"
)

var (
	ErrEmptyBody   = errors.New("agent: empty message")
	ErrNotFound    = errors.New("agent: not found")
	ErrForbidden   = errors.New("agent: forbidden")
	ErrDisabled    = errors.New("agent: AI is not configured for this community")
	ErrGenerating  = errors.New("agent: a reply is already being generated")
	ErrBadProvider = errors.New("agent: unknown provider")
)

// Config is one community's AI settings (one row in ai_configs).
type Config struct {
	CommunityID  string
	Provider     string
	BaseURL      string
	Model        string
	APIKeyEnc    string
	SystemPrompt string
	Enabled      bool
	UpdatedBy    string
	UpdatedAt    int64
}

// Thread is one conversation with the agent.
type Thread struct {
	ID          string
	CommunityID string
	UserID      string // creator
	Visibility  string
	Title       string
	Model       string
	CreatedAt   int64
	UpdatedAt   int64
}

// Message is one turn in a thread.
type Message struct {
	ID        string
	ThreadID  string
	Role      string
	AuthorID  string // member who typed a user turn; "" for assistant/system
	BodyMD    string
	BodyHTML  string
	Status    string
	Error     string
	CreatedAt int64
	UpdatedAt int64
}
