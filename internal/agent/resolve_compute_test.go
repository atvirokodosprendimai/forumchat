package agent

import (
	"context"
	"testing"
)

// TestResolveProvider_OverrideAndDefault verifies the compute seam: a nil
// resolver leaves the agent on its own provider/model (BYO, self-host), while a
// resolver may override the agent that the generation runs with — load-bearing,
// because Generate streams against the returned Agent.Model.
func TestResolveProvider_OverrideAndDefault(t *testing.T) {
	byo := Agent{Provider: ProviderOllama, BaseURL: "http://byo:11434", Model: "byo-model"}

	// nil resolver → bare provider, agent unchanged.
	if _, a, err := resolveProvider(context.Background(), nil, "c1", byo); err != nil || a.Model != "byo-model" {
		t.Fatalf("nil resolver must keep BYO model, got model=%q err=%v", a.Model, err)
	}

	// resolver overrides the agent's compute (e.g. platform vision model).
	rsv := func(ctx context.Context, communityID string, in Agent) (Provider, Agent, error) {
		in.BaseURL, in.Model = "http://platform:11434", "gemma4"
		return NewOllama(in.BaseURL), in, nil
	}
	_, a, err := resolveProvider(context.Background(), rsv, "c1", byo)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if a.Model != "gemma4" || a.BaseURL != "http://platform:11434" {
		t.Fatalf("resolver must override compute, got %+v", a)
	}
}
