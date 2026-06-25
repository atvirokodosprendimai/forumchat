package community

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// RiskSignals are the privacy-preserving abuse indicators for one community.
// Every field is an AGGREGATE COUNT or RATIO computed entirely inside SQL — no
// message body, member identity, or any other tenant content ever leaves the
// database. This is the deliberate trade behind the SaaS privacy wall: the
// platform operator can no longer read a tenant's content (see
// auth.Identity.GodMode), so botnet/abuse detection must work from metadata
// alone. The duplicate-content signal groups by body_md INSIDE SQL but selects
// only a count — the operator sees "73% of messages are duplicates", never the
// messages themselves.
type RiskSignals struct {
	MembersTotal  int // all membership rows
	MembersNew24h int // memberships created in the last 24h
	Messages24h   int // non-deleted user chat messages in the last 24h
	Messages1h    int // ... in the last 1h (burst detection)
	Authors24h    int // distinct authors of those 24h messages
	// DupExcess24h is the number of REDUNDANT message copies in the last 24h:
	// the sum over identical-body groups of (count-1). Zero when every message
	// is unique; high under copypasta spam.
	DupExcess24h int
}

// CommunityRisk pairs a community with its signals and the computed assessment.
type CommunityRisk struct {
	Community
	Signals    RiskSignals
	Assessment RiskAssessment
}

// RiskAssessment is the scored verdict derived purely from RiskSignals.
type RiskAssessment struct {
	Score   int      // 0..100; higher = more botnet-like
	Band    string   // "low" | "elevated" | "high"
	Reasons []string // human-readable signals that fired (counts only, no content)
}

// Risk bands. Kept here so the handler/templ share one definition.
const (
	RiskLow      = "low"
	RiskElevated = "elevated"
	RiskHigh     = "high"
)

// ScoreRisk turns raw metadata signals into an interpretable 0..100 risk score,
// a band, and the list of reasons that fired. It is a PURE function (no DB, no
// clock) so the weighting is unit-testable and the thresholds are auditable in
// one place. The weights are heuristic, deliberately conservative (each signal
// has a non-trivial floor so a small or brand-new community doesn't trip it),
// and additive so several weak signals together still raise a flag.
func ScoreRisk(s RiskSignals) RiskAssessment {
	var score int
	var reasons []string

	// 1. New-account surge — a botnet mass-registers. Score on the FRACTION of
	//    the member base that joined in 24h, gated by an absolute floor so an
	//    organic launch spike on a tiny community isn't flagged.
	if s.MembersNew24h >= 10 && s.MembersTotal > 0 {
		frac := float64(s.MembersNew24h) / float64(s.MembersTotal)
		switch {
		case frac >= 0.5:
			score += 35
			reasons = append(reasons, fmt.Sprintf("%d new members in 24h (%.0f%% of the community)", s.MembersNew24h, frac*100))
		case frac >= 0.25:
			score += 20
			reasons = append(reasons, fmt.Sprintf("%d new members in 24h", s.MembersNew24h))
		}
	}

	// 2. Author concentration — a few accounts producing a flood. Messages per
	//    active author over 24h, gated by an absolute volume floor.
	if s.Messages24h >= 50 && s.Authors24h > 0 {
		perAuthor := float64(s.Messages24h) / float64(s.Authors24h)
		switch {
		case perAuthor >= 100:
			score += 25
			reasons = append(reasons, fmt.Sprintf("%.0f messages per active author in 24h", perAuthor))
		case perAuthor >= 40:
			score += 12
			reasons = append(reasons, "high message volume per author")
		}
	}

	// 3. Duplicate-content ratio — copypasta spam. Fraction of 24h messages that
	//    are redundant copies. Body text is never surfaced, only this ratio.
	if s.Messages24h >= 20 {
		dup := float64(s.DupExcess24h) / float64(s.Messages24h)
		switch {
		case dup >= 0.5:
			score += 30
			reasons = append(reasons, fmt.Sprintf("%.0f%% of 24h messages are duplicate copies", dup*100))
		case dup >= 0.25:
			score += 15
			reasons = append(reasons, "elevated duplicate-message ratio")
		}
	}

	// 4. Message burst — the last hour far above the 24h hourly average. A
	//    smaller signal (volume alone isn't abuse), so it only nudges the score.
	if s.Messages1h >= 30 && s.Messages24h > 0 {
		avgHour := float64(s.Messages24h) / 24.0
		if avgHour > 0 && float64(s.Messages1h) >= 5*avgHour {
			score += 10
			reasons = append(reasons, fmt.Sprintf("%d messages in the last hour (5x+ the daily rate)", s.Messages1h))
		}
	}

	if score > 100 {
		score = 100
	}
	band := RiskLow
	switch {
	case score >= 70:
		band = RiskHigh
	case score >= 40:
		band = RiskElevated
	}
	return RiskAssessment{Score: score, Band: band, Reasons: reasons}
}

// RiskSignals computes the abuse signals for every community as of now and
// returns them sorted by risk score, highest first. The clock is a parameter
// so callers/tests pin the 1h/24h windows. Each signal is a COUNT/ratio over
// existing tables — there is no schema change and nothing tenant-private is
// read out. Mirrors ListAll's correlated-subquery shape.
func (r *Repo) RiskSignals(ctx context.Context, now time.Time) ([]CommunityRisk, error) {
	t24 := now.Add(-24 * time.Hour).Unix()
	t1 := now.Add(-1 * time.Hour).Unix()
	// Column order fixes the placeholder order: members_new(t24), msgs24(t24),
	// msgs1h(t1), authors(t24), dup_excess(t24). The duplicate subquery groups
	// by body_md but selects only COUNT — body text never leaves SQLite.
	rows, err := r.DB.QueryContext(ctx, `
		SELECT c.id, c.slug, c.name, COALESCE(c.is_public,0), c.created_at,
		       (SELECT COUNT(*) FROM memberships mb WHERE mb.community_id = c.id) AS members_total,
		       (SELECT COUNT(*) FROM memberships mb WHERE mb.community_id = c.id AND mb.created_at >= ?) AS members_new_24h,
		       (SELECT COUNT(*) FROM chat_messages cm WHERE cm.community_id = c.id AND cm.deleted_at IS NULL AND cm.kind = 'user' AND cm.created_at >= ?) AS msgs_24h,
		       (SELECT COUNT(*) FROM chat_messages cm WHERE cm.community_id = c.id AND cm.deleted_at IS NULL AND cm.kind = 'user' AND cm.created_at >= ?) AS msgs_1h,
		       (SELECT COUNT(DISTINCT cm.author_id) FROM chat_messages cm WHERE cm.community_id = c.id AND cm.deleted_at IS NULL AND cm.kind = 'user' AND cm.created_at >= ?) AS authors_24h,
		       (SELECT COALESCE(SUM(cnt - 1), 0) FROM (
		            SELECT COUNT(*) AS cnt FROM chat_messages cm
		             WHERE cm.community_id = c.id AND cm.deleted_at IS NULL AND cm.kind = 'user' AND cm.created_at >= ?
		             GROUP BY cm.body_md HAVING COUNT(*) > 1) g) AS dup_excess_24h
		FROM communities c
		ORDER BY c.created_at DESC`, t24, t24, t1, t24, t24)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommunityRisk
	for rows.Next() {
		var cr CommunityRisk
		var created int64
		var isPublic int
		if err := rows.Scan(&cr.Community.ID, &cr.Community.Slug, &cr.Community.Name,
			&isPublic, &created,
			&cr.Signals.MembersTotal, &cr.Signals.MembersNew24h,
			&cr.Signals.Messages24h, &cr.Signals.Messages1h,
			&cr.Signals.Authors24h, &cr.Signals.DupExcess24h); err != nil {
			return nil, err
		}
		cr.Community.IsPublic = isPublic != 0
		cr.Community.CreatedAt = time.Unix(created, 0)
		cr.Assessment = ScoreRisk(cr.Signals)
		out = append(out, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Highest risk first; stable on slug for deterministic ties (and tests).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Assessment.Score != out[j].Assessment.Score {
			return out[i].Assessment.Score > out[j].Assessment.Score
		}
		return out[i].Community.Slug < out[j].Community.Slug
	})
	return out, nil
}
