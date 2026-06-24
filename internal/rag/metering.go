package rag

import (
	"context"

	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
)

// metering.go holds the platform-AI metering decorator for RAG embedding. It
// wraps a bare Embedder and appends one aiusage ledger row per Embed batch,
// installed ONLY on the platform-compute branch of the resolver (Phase 2). A BYO
// community keeps the bare embedder, so "meter iff platform" is structural.

// meteredEmbedder wraps an Embedder, recording the (estimated) input tokens of
// each batch it embeds for communityID. Ollama's /api/embed reports no usage, so
// tokens are estimated from input length (Estimated=true). Embedding is
// background work (the outbox worker), so no user is attributed.
type meteredEmbedder struct {
	inner       Embedder
	rec         *aiusage.Recorder
	communityID string
}

// NewMeteredEmbedder wraps inner so every batch it embeds for communityID is
// recorded to rec (nil-safe). Returns inner unwrapped when rec or communityID is
// absent, so the bare BYO path never pays for a decorator.
func NewMeteredEmbedder(inner Embedder, rec *aiusage.Recorder, communityID string) Embedder {
	if rec == nil || communityID == "" {
		return inner
	}
	return &meteredEmbedder{inner: inner, rec: rec, communityID: communityID}
}

func (m *meteredEmbedder) Dim() int      { return m.inner.Dim() }
func (m *meteredEmbedder) Model() string { return m.inner.Model() }

func (m *meteredEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vecs, err := m.inner.Embed(ctx, texts)
	if err != nil || len(texts) == 0 {
		return vecs, err
	}
	var tokens int
	for _, t := range texts {
		tokens += aiusage.EstimateTokens(t)
	}
	m.rec.Record(ctx, aiusage.Event{
		CommunityID: m.communityID,
		Feature:     aiusage.FeatureRAGEmbed,
		Model:       m.inner.Model(),
		TokensIn:    tokens,
		Estimated:   true,
	})
	return vecs, err
}
