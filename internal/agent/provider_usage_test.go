package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOllamaStream_ParsesUsage verifies the streaming path extracts Ollama's
// prompt_eval_count / eval_count from the final done object into StreamResult.Usage
// — the token counts the platform-AI metering decorator bills on.
func TestOllamaStream_ParsesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		io.WriteString(w, `{"message":{"content":"Hel"},"done":false}`+"\n")
		io.WriteString(w, `{"message":{"content":"lo"},"done":false}`+"\n")
		io.WriteString(w, `{"message":{"content":""},"done":true,"prompt_eval_count":42,"eval_count":7}`+"\n")
	}))
	defer srv.Close()

	o := NewOllama(srv.URL)
	var got strings.Builder
	res, err := o.Stream(context.Background(), "m", []ChatMessage{{Role: RoleUser, Content: "hi"}}, nil,
		func(d string) error { got.WriteString(d); return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if got.String() != "Hello" {
		t.Fatalf("content = %q, want %q", got.String(), "Hello")
	}
	if res.Usage.PromptTokens != 42 || res.Usage.CompletionTokens != 7 {
		t.Fatalf("usage = %+v, want {42 7}", res.Usage)
	}
}
