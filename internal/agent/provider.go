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

// ChatMessage is one turn handed to a Provider. Role is system|user|assistant.
// Images are base64-encoded image payloads (no data: prefix) attached to a
// user turn — Ollama's /api/chat accepts them on the message; omitted when empty.
type ChatMessage struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

// Provider streams an assistant completion. Stream calls onDelta for each
// content chunk as it arrives and returns when the model finishes or ctx is
// cancelled. Implementations MUST respect ctx and MUST NOT call onDelta after
// returning. Keeping the wire format here means a Claude/OpenAI provider drops
// in by implementing this one method.
type Provider interface {
	Name() string
	Stream(ctx context.Context, model string, msgs []ChatMessage, onDelta func(string) error) error
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

type ollamaChatReq struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ollamaChatChunk struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Done  bool   `json:"done"`
	Error string `json:"error"`
}

// Stream POSTs to /api/chat and forwards each content chunk to onDelta until
// the daemon reports done, ctx is cancelled, or an error surfaces.
func (o *Ollama) Stream(ctx context.Context, model string, msgs []ChatMessage, onDelta func(string) error) error {
	payload, err := json.Marshal(ollamaChatReq{Model: model, Messages: msgs, Stream: true})
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("call ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("ollama %s: %s", resp.Status, strings.TrimSpace(string(buf[:n])))
	}

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
			return fmt.Errorf("ollama: %s", chunk.Error)
		}
		if chunk.Message.Content != "" {
			if err := onDelta(chunk.Message.Content); err != nil {
				return err
			}
		}
		if chunk.Done {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read stream: %w", err)
	}
	return nil
}

// nowUnix is the package time source, isolated for testability.
func nowUnix() int64 { return time.Now().Unix() }
