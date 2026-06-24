// Package aiusage is the platform-AI metering ledger: an append-only record of
// every request a community ran on the PLATFORM'S OWN AI compute (the "use
// system-wide settings" opt-in), dimensioned by feature + community (+ the
// triggering user) and measured in tokens in/out. See
// eidos/spec - saas-platform-ai …
//
// It is a leaf infrastructure package — it imports nothing else in this codebase
// (only database/sql + uuid), so any subsystem may record into it without an
// import cycle. The metering decorators in internal/agent and internal/rag wrap
// the platform compute clients and call Record; BYO clients are left bare and
// record nothing, which makes "meter iff platform" a structural property.
//
// The Recorder is nil-safe (a nil *Recorder no-ops), mirroring
// internal/debuglog.Recorder, so a deployment with platform AI unwired carries a
// nil recorder and call sites need no guard.
package aiusage

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Feature labels the AI subsystem that served a metered request.
const (
	FeatureAgent     = "agent"
	FeatureRAGEmbed  = "rag_embed"
	FeatureTranslate = "translate"
)

// Event is one metered platform-compute request, ready to append to the ledger.
// UserID is empty for background work (e.g. the embed worker draining historical
// content); Estimated is true when token counts were derived from input length
// rather than reported by the provider (embeddings / translation).
type Event struct {
	CommunityID string
	Feature     string
	UserID      string
	Model       string
	TokensIn    int
	TokensOut   int
	Estimated   bool
}

// Recorder appends usage events to the ai_usage_events table and reads back
// rollups for the usage panels. The zero value is not usable — construct with
// New. All methods are safe on a nil receiver.
type Recorder struct {
	db  *sql.DB
	log *slog.Logger
}

// New returns a Recorder backed by db.
func New(db *sql.DB, log *slog.Logger) *Recorder { return &Recorder{db: db, log: log} }

// Record appends one usage event. It is a cheap no-op (and nil-safe) when the
// recorder is unwired, and silently drops events missing the mandatory
// community/feature dimensions. A write failure is logged, never returned —
// metering must never fail the request it is observing (the ledger is
// eventually-correct, not transactional with the generation).
func (r *Recorder) Record(ctx context.Context, e Event) {
	if r == nil || r.db == nil {
		return
	}
	if e.CommunityID == "" || e.Feature == "" {
		return
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO ai_usage_events
		   (id, community_id, feature, user_id, model, tokens_in, tokens_out, estimated, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), e.CommunityID, e.Feature, nullStr(e.UserID), e.Model,
		e.TokensIn, e.TokensOut, boolInt(e.Estimated), time.Now().Unix())
	if err != nil && r.log != nil {
		r.log.Warn("aiusage: record", "err", err, "community", e.CommunityID, "feature", e.Feature)
	}
}

// FeatureTotal is one feature's aggregate usage for a community over a window.
type FeatureTotal struct {
	Feature   string
	Requests  int
	TokensIn  int
	TokensOut int
}

// Rollup returns per-feature totals for one community between [from, to] (Unix
// seconds, inclusive), ordered by feature. An empty result is not an error.
func (r *Recorder) Rollup(ctx context.Context, communityID string, from, to int64) ([]FeatureTotal, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT feature, COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		   FROM ai_usage_events
		  WHERE community_id = ? AND created_at BETWEEN ? AND ?
		  GROUP BY feature
		  ORDER BY feature`, communityID, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeatureTotal
	for rows.Next() {
		var ft FeatureTotal
		if err := rows.Scan(&ft.Feature, &ft.Requests, &ft.TokensIn, &ft.TokensOut); err != nil {
			return nil, err
		}
		out = append(out, ft)
	}
	return out, rows.Err()
}

// CommunityTotal is one community's grand aggregate usage over a window — the
// row the super-admin cost panel lists.
type CommunityTotal struct {
	CommunityID string
	Requests    int
	TokensIn    int
	TokensOut   int
}

// CommunityTotals returns grand totals per community between [from, to] (Unix
// seconds, inclusive), busiest first. This is the operator's fleet-wide cost
// view.
func (r *Recorder) CommunityTotals(ctx context.Context, from, to int64) ([]CommunityTotal, error) {
	if r == nil || r.db == nil {
		return nil, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT community_id, COUNT(*), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0)
		   FROM ai_usage_events
		  WHERE created_at BETWEEN ? AND ?
		  GROUP BY community_id
		  ORDER BY SUM(tokens_in) + SUM(tokens_out) DESC`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommunityTotal
	for rows.Next() {
		var ct CommunityTotal
		if err := rows.Scan(&ct.CommunityID, &ct.Requests, &ct.TokensIn, &ct.TokensOut); err != nil {
			return nil, err
		}
		out = append(out, ct)
	}
	return out, rows.Err()
}

// EstimateTokens approximates the token count of text for providers that don't
// report usage — Ollama's /api/embed and the translation turn. It uses the same
// ~4-chars-per-token heuristic as the RAG chunker; events recorded with it carry
// Estimated=true. Exact counts replace it when hosted LLM providers land.
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	n := utf8.RuneCountInString(text) / 4
	if n == 0 {
		return 1
	}
	return n
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
