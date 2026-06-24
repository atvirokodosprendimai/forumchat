package agent

import (
	"context"

	"github.com/atvirokodosprendimai/forumchat/internal/aiusage"
)

// metering.go holds the platform-AI metering decorators for agent compute. They
// wrap the bare provider / translate call and append one aiusage ledger row per
// request. They are installed ONLY on the platform-compute branch of the
// resolver (Phase 2 wiring in main.go) — a BYO community keeps the bare client,
// so "meter iff platform" is structural, not a per-call-site discipline.

// meteredProvider wraps a Provider, recording the token usage of each turn it
// streams against communityID (triggered by userID, "" for none). It records one
// row per provider turn; a multi-turn agentic loop sums naturally over the rows
// at rollup time. The wrapped provider's behaviour is otherwise unchanged.
type meteredProvider struct {
	inner       Provider
	rec         *aiusage.Recorder
	communityID string
	userID      string
}

// NewMeteredProvider wraps inner so every turn it streams for communityID is
// recorded to rec (nil-safe). Returns inner unwrapped when rec or communityID is
// absent, so the bare BYO path never pays for a decorator.
func NewMeteredProvider(inner Provider, rec *aiusage.Recorder, communityID, userID string) Provider {
	if rec == nil || communityID == "" {
		return inner
	}
	return &meteredProvider{inner: inner, rec: rec, communityID: communityID, userID: userID}
}

func (m *meteredProvider) Name() string { return m.inner.Name() }

func (m *meteredProvider) Stream(ctx context.Context, model string, msgs []ChatMessage, tools []ToolDef, onDelta func(string) error) (*StreamResult, error) {
	res, err := m.inner.Stream(ctx, model, msgs, tools, onDelta)
	if res != nil {
		m.rec.Record(ctx, aiusage.Event{
			CommunityID: m.communityID,
			Feature:     aiusage.FeatureAgent,
			UserID:      m.userID,
			Model:       model,
			TokensIn:    res.Usage.PromptTokens,
			TokensOut:   res.Usage.CompletionTokens,
		})
	}
	return res, err
}

// MeteredTranslate runs a translation on platform compute and records its
// (estimated) token usage to rec against communityID/userID. The translation
// turn returns no provider usage, so input/output tokens are estimated from text
// length (Estimated=true). On the BYO path callers use Translate directly and
// nothing is recorded. rec is nil-safe.
func MeteredTranslate(ctx context.Context, rec *aiusage.Recorder, communityID, userID, baseURL, model, text string) ([]string, error) {
	out, err := Translate(ctx, baseURL, model, text)
	if err != nil {
		return out, err
	}
	var outTokens int
	for _, t := range out {
		outTokens += aiusage.EstimateTokens(t)
	}
	rec.Record(ctx, aiusage.Event{
		CommunityID: communityID,
		Feature:     aiusage.FeatureTranslate,
		UserID:      userID,
		Model:       model,
		TokensIn:    aiusage.EstimateTokens(text),
		TokensOut:   outTokens,
		Estimated:   true,
	})
	return out, err
}
