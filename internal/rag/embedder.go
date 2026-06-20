package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Ollama is an Embedder backed by an Ollama daemon's /api/embed endpoint, which
// accepts a batch of inputs and returns one vector each. We talk to it directly
// (no dependency) for the same reason internal/agent/provider.go does — small,
// explicit wire shape. The dimension is reported by the caller (config), since
// /api/embed doesn't echo it until the first call.
type Ollama struct {
	BaseURL string
	ModelID string
	DimN    int
	HTTP    *http.Client
}

// NewOllamaEmbedder points an embedder at baseURL (e.g. http://localhost:11434)
// using model (e.g. bge-m3) producing dim-dimensional vectors. A generous client
// timeout guards a hung daemon without aborting a legitimately slow batch.
func NewOllamaEmbedder(baseURL, model string, dim int) *Ollama {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = "http://localhost:11434"
	}
	return &Ollama{
		BaseURL: base,
		ModelID: strings.TrimSpace(model),
		DimN:    dim,
		HTTP:    &http.Client{Timeout: 2 * time.Minute},
	}
}

func (o *Ollama) Dim() int      { return o.DimN }
func (o *Ollama) Model() string { return o.ModelID }

type ollamaEmbedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResp struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

// Embed returns one vector per input, in order. An empty batch is a no-op.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	payload, err := json.Marshal(ollamaEmbedReq{Model: o.ModelID, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/embed", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call ollama embed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		return nil, fmt.Errorf("ollama embed %s: %s", resp.Status, strings.TrimSpace(string(buf[:n])))
	}

	var out ollamaEmbedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode embed response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("ollama embed: %s", out.Error)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: got %d vectors for %d inputs", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}
