package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ChatMessage is one turn handed to a Provider. Role is system|user|assistant|tool.
// Images are base64-encoded image payloads (no data: prefix) attached to a
// user turn — Ollama's /api/chat accepts them on the message; omitted when empty.
// ToolCalls is set on an assistant turn that requested tools (so the next model
// turn sees its own request); ToolName labels a role="tool" result turn.
type ChatMessage struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	Images    []string   `json:"images,omitempty"`
	ToolCalls []ToolCall `json:"-"`
	ToolName  string     `json:"-"`
}

// StreamResult reports the outcome of one provider turn. When ToolCalls is
// non-empty the model paused to call tools (no content was streamed this turn);
// the runner executes them, appends the results, and calls Stream again. When
// empty, the assistant content was streamed via onDelta and the turn is final.
type StreamResult struct {
	ToolCalls []ToolCall
}

// Provider runs one assistant turn. With no tools it streams the answer via
// onDelta (StreamResult.ToolCalls empty). With tools it MAY instead return tool
// calls for the runner to execute (the agentic loop lives in the runner, not
// here). Implementations MUST respect ctx and MUST NOT call onDelta after
// returning. A Claude/OpenAI provider drops in by implementing this one method.
type Provider interface {
	Name() string
	Stream(ctx context.Context, model string, msgs []ChatMessage, tools []ToolDef, onDelta func(string) error) (*StreamResult, error)
}

// newProvider selects the Provider for an agent. Ollama needs no key; the
// Claude/OpenAI branches land here later, reading a.APIKeyEnc.
func newProvider(a Agent) (Provider, error) {
	switch a.Provider {
	case ProviderOllama, "":
		return NewOllama(a.BaseURL), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrBadProvider, a.Provider)
	}
}

// Ollama talks to a local/remote Ollama daemon's /api/chat with stream:true,
// which returns newline-delimited JSON objects. We implement the client
// directly (rather than pulling a dependency) because the 100ms buffered
// flush needs raw control over the per-chunk token stream.
type Ollama struct {
	BaseURL string
	HTTP    *http.Client
}

// NewOllama returns an Ollama provider pointed at baseURL (e.g.
// http://localhost:11434). No request timeout is set on the client — a long
// generation is normal; cancellation flows through the request context.
func NewOllama(baseURL string) *Ollama {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	return &Ollama{BaseURL: base, HTTP: &http.Client{}}
}

func (o *Ollama) Name() string { return ProviderOllama }

// Ollama wire shapes. We translate the provider-agnostic ChatMessage into these
// so tool calls / tool results / tool defs marshal exactly as /api/chat expects.

type ollamaToolCallFn struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type ollamaToolCall struct {
	Function ollamaToolCallFn `json:"function"`
}
type ollamaMsg struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	ToolName  string           `json:"tool_name,omitempty"`
}
type ollamaToolFn struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}
type ollamaTool struct {
	Type     string       `json:"type"`
	Function ollamaToolFn `json:"function"`
}
type ollamaChatReq struct {
	Model    string       `json:"model"`
	Messages []ollamaMsg  `json:"messages"`
	Tools    []ollamaTool `json:"tools,omitempty"`
	Stream   bool         `json:"stream"`
}

type ollamaChatChunk struct {
	Message struct {
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done  bool   `json:"done"`
	Error string `json:"error"`
}

func toOllamaMsgs(msgs []ChatMessage) []ollamaMsg {
	out := make([]ollamaMsg, 0, len(msgs))
	for _, m := range msgs {
		om := ollamaMsg{Role: m.Role, Content: m.Content, Images: m.Images, ToolName: m.ToolName}
		for _, c := range m.ToolCalls {
			args := c.Args
			if len(args) == 0 {
				args = json.RawMessage("{}")
			}
			om.ToolCalls = append(om.ToolCalls, ollamaToolCall{Function: ollamaToolCallFn{Name: c.Name, Arguments: args}})
		}
		out = append(out, om)
	}
	return out
}

func toOllamaTools(tools []ToolDef) []ollamaTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ollamaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, ollamaTool{Type: "function", Function: ollamaToolFn{
			Name: t.Name, Description: t.Description, Parameters: t.Schema,
		}})
	}
	return out
}

// Stream POSTs to /api/chat. With no tools it streams content chunks to onDelta
// until done. With tools it runs non-streamed (Ollama returns tool calls or the
// final message as one object): if the model requested tools they're returned in
// the StreamResult for the runner to execute; otherwise the content is emitted
// once via onDelta and the turn is final.
func (o *Ollama) Stream(ctx context.Context, model string, msgs []ChatMessage, tools []ToolDef, onDelta func(string) error) (*StreamResult, error) {
	streaming := len(tools) == 0
	payload, err := json.Marshal(ollamaChatReq{
		Model: model, Messages: toOllamaMsgs(msgs), Tools: toOllamaTools(tools), Stream: streaming,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		return nil, fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(buf[:n])))
	}

	res := &StreamResult{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk ollamaChatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue // skip malformed keep-alive lines
		}
		if chunk.Error != "" {
			return nil, fmt.Errorf("ollama: %s", chunk.Error)
		}
		for _, tc := range chunk.Message.ToolCalls {
			res.ToolCalls = append(res.ToolCalls, ToolCall{Name: tc.Function.Name, Args: tc.Function.Arguments})
		}
		if chunk.Message.Content != "" {
			if err := onDelta(chunk.Message.Content); err != nil {
				return nil, err
			}
		}
		if chunk.Done {
			return res, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	return res, nil
}

// nowUnix is the package time source, isolated for testability.
func nowUnix() int64 { return time.Now().Unix() }
