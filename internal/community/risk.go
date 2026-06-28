package community

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// dedupeCSV splits a comma-joined list (GROUP_CONCAT of per-row category CSVs,
// e.g. "S3,S12,S3"), trims, drops blanks, de-duplicates, and returns the codes
// in a DETERMINISTIC order. SQLite's GROUP_CONCAT order is unspecified, so we
// sort: hazard codes ("S<n>") by their numeric suffix (S3 before S12), and any
// non-code values lexically after. Returns nil for empty input.
func dedupeCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ni, oki := codeNum(out[i])
		nj, okj := codeNum(out[j])
		if oki && okj {
			return ni < nj
		}
		if oki != okj {
			return oki // numeric codes sort before non-codes
		}
		return out[i] < out[j]
	})
	return out
}

// codeNum extracts the integer from a hazard code like "S12" → (12, true).
// Returns (0, false) for anything not of that shape.
func codeNum(code string) (int, bool) {
	if len(code) < 2 || (code[0] != 'S' && code[0] != 's') {
		return 0, false
	}
	n := 0
	for _, r := range code[1:] {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

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
	// Flagged24h is the count of messages the automated safety classifier
	// flagged in the last 24h (internal/moderation; zero when that feature is
	// off). FlaggedAuthors24h is how many DISTINCT authors those flags span —
	// the signal that separates one bad actor from a coordinated community.
	// FlaggedCategories are the distinct Llama Guard hazard CODES seen (e.g.
	// "S12"); the codes are kept taxonomy-agnostic here and mapped to labels by
	// the caller, so this package stays decoupled from internal/moderation.
	Flagged24h        int
	FlaggedAuthors24h int
	FlaggedCategories []string
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
	var floor int // a minimum the strongest signals pin the score to
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

	// 5. Content flagged by the automated safety classifier (Phase B). The
	//    strongest signal — this is actual policy-violating content, not just
	//    volume. The author spread distinguishes one bad actor from a community
	//    that is collectively abusive. Category labels are added by the caller;
	//    the reason here stays code-free so this package needn't know the
	//    taxonomy.
	if s.Flagged24h > 0 {
		// Fraction of 24h traffic that was flagged — so a small flagged burst
		// can't pin a busy community (FIX1 N4). Guard div-by-zero.
		flaggedRatio := 0.0
		if s.Messages24h > 0 {
			flaggedRatio = float64(s.Flagged24h) / float64(s.Messages24h)
		}
		switch {
		case s.FlaggedAuthors24h >= 3 && flaggedRatio >= 0.2:
			// Genuinely coordinated abuse: flagged content spread across several
			// accounts AND a significant share of traffic. ONLY this pins "high".
			// A single attacker (FlaggedAuthors24h==1) can no longer force the
			// floor by spamming flagged messages, and three sockpuppets can't pin
			// a busy channel where their flags are a tiny fraction of traffic.
			score += 45
			floor = 70
			reasons = append(reasons, fmt.Sprintf("%d messages auto-flagged by the safety classifier from %d authors in 24h (coordinated)", s.Flagged24h, s.FlaggedAuthors24h))
		case s.Flagged24h >= 10 || s.FlaggedAuthors24h >= 3:
			// Strong but not-clearly-coordinated: scale the score WITHOUT flooring,
			// so it can combine with the other heuristics to reach high organically
			// but a lone actor can't dictate the band.
			score += 35
			reasons = append(reasons, fmt.Sprintf("%d messages auto-flagged by the safety classifier from %d author(s) in 24h", s.Flagged24h, s.FlaggedAuthors24h))
		default:
			score += 25
			reasons = append(reasons, fmt.Sprintf("%d message(s) auto-flagged by the safety classifier from %d author(s) in 24h", s.Flagged24h, s.FlaggedAuthors24h))
		}
	}

	if score < floor {
		score = floor
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
		             GROUP BY cm.body_md HAVING COUNT(*) > 1) g) AS dup_excess_24h,
		       (SELECT COUNT(*) FROM moderation_flags mf WHERE mf.community_id = c.id AND mf.created_at >= ?) AS flagged_24h,
		       (SELECT COUNT(DISTINCT mf.author_id) FROM moderation_flags mf WHERE mf.community_id = c.id AND mf.created_at >= ?) AS flagged_authors_24h,
		       (SELECT COALESCE(GROUP_CONCAT(mf.categories), '') FROM moderation_flags mf WHERE mf.community_id = c.id AND mf.created_at >= ?) AS flagged_cats_24h
		FROM communities c
		ORDER BY c.created_at DESC`, t24, t24, t1, t24, t24, t24, t24, t24)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommunityRisk
	for rows.Next() {
		var cr CommunityRisk
		var created int64
		var isPublic int
		var flaggedCats string // GROUP_CONCAT of per-row category CSVs
		if err := rows.Scan(&cr.Community.ID, &cr.Community.Slug, &cr.Community.Name,
			&isPublic, &created,
			&cr.Signals.MembersTotal, &cr.Signals.MembersNew24h,
			&cr.Signals.Messages24h, &cr.Signals.Messages1h,
			&cr.Signals.Authors24h, &cr.Signals.DupExcess24h,
			&cr.Signals.Flagged24h, &cr.Signals.FlaggedAuthors24h, &flaggedCats); err != nil {
			return nil, err
		}
		cr.Community.IsPublic = isPublic != 0
		cr.Community.CreatedAt = time.Unix(created, 0)
		cr.Signals.FlaggedCategories = dedupeCSV(flaggedCats)
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
