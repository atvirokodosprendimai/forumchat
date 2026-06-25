package community

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// mustUser inserts a minimal active user row so chat_messages.author_id (FK to
// users) is satisfied.
func mustUser(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO users (id, email, password_hash, status, created_at, updated_at)
		VALUES (?, ?, 'x', 'active', 0, 0)`, id, id+"@t.test")
	if err != nil {
		t.Fatalf("insert user %s: %v", id, err)
	}
}

// insMsg inserts one non-deleted user chat message at a given time.
func insMsg(t *testing.T, db *sql.DB, id, cid, author, body string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO chat_messages (id, community_id, author_id, kind, body_md, body_html, created_at)
		VALUES (?, ?, ?, 'user', ?, '', ?)`, id, cid, author, body, at.Unix())
	if err != nil {
		t.Fatalf("insert msg %s: %v", id, err)
	}
}

func TestScoreRisk_Bands(t *testing.T) {
	cases := []struct {
		name     string
		sig      RiskSignals
		wantBand string
		wantFire bool // at least one reason
	}{
		{
			name:     "healthy community",
			sig:      RiskSignals{MembersTotal: 200, MembersNew24h: 2, Messages24h: 80, Messages1h: 4, Authors24h: 30, DupExcess24h: 1},
			wantBand: RiskLow,
			wantFire: false,
		},
		{
			name:     "mass signup + copypasta = high",
			sig:      RiskSignals{MembersTotal: 100, MembersNew24h: 80, Messages24h: 400, Messages1h: 60, Authors24h: 3, DupExcess24h: 300},
			wantBand: RiskHigh,
			wantFire: true,
		},
		{
			name:     "moderate duplicate ratio = elevated",
			sig:      RiskSignals{MembersTotal: 100, MembersNew24h: 1, Messages24h: 100, Messages1h: 5, Authors24h: 20, DupExcess24h: 30},
			wantBand: RiskLow, // 15 points only — below elevated floor
			wantFire: true,
		},
		{
			name:     "tiny new community is NOT flagged",
			sig:      RiskSignals{MembersTotal: 4, MembersNew24h: 4, Messages24h: 10, Messages1h: 10, Authors24h: 1, DupExcess24h: 5},
			wantBand: RiskLow,
			wantFire: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScoreRisk(c.sig)
			if got.Band != c.wantBand {
				t.Errorf("band = %q (score %d), want %q; reasons=%v", got.Band, got.Score, c.wantBand, got.Reasons)
			}
			if (len(got.Reasons) > 0) != c.wantFire {
				t.Errorf("reasons fired = %v, want %v (reasons=%v)", len(got.Reasons) > 0, c.wantFire, got.Reasons)
			}
			if got.Score < 0 || got.Score > 100 {
				t.Errorf("score out of range: %d", got.Score)
			}
		})
	}
}

// TestRiskSignals_AggregatesAndDuplicates seeds two communities and asserts the
// SQL aggregation: 24h/1h windows, distinct authors, and the duplicate-excess
// count over identical bodies — without selecting any body text.
func TestRiskSignals_AggregatesAndDuplicates(t *testing.T) {
	ctx := context.Background()
	r := newTestRepo(t)
	now := time.Unix(1_700_000_000, 0)

	clean, err := r.Create(ctx, "clean", "Clean")
	if err != nil {
		t.Fatalf("create clean: %v", err)
	}
	bot, err := r.Create(ctx, "bot", "Botnet")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	// Two users to author messages (author_id has a FK to users).
	mustUser(t, r.DB, "ua")
	mustUser(t, r.DB, "ub")

	// Clean: 3 unique recent messages by two authors.
	insMsg(t, r.DB, "c1", clean.ID, "ua", "hello", now.Add(-10*time.Minute))
	insMsg(t, r.DB, "c2", clean.ID, "ub", "world", now.Add(-20*time.Minute))
	insMsg(t, r.DB, "c3", clean.ID, "ua", "again", now.Add(-2*time.Hour))

	// Bot: 5 identical-body messages in the last hour by one author (4 redundant
	// copies), plus one old message outside the 24h window (must NOT count).
	for i, id := range []string{"b1", "b2", "b3", "b4", "b5"} {
		insMsg(t, r.DB, id, bot.ID, "ua", "JOIN MY CHANNEL", now.Add(-time.Duration(i+1)*time.Minute))
	}
	insMsg(t, r.DB, "bold", bot.ID, "ua", "ancient", now.Add(-48*time.Hour))

	rows, err := r.RiskSignals(ctx, now)
	if err != nil {
		t.Fatalf("RiskSignals: %v", err)
	}
	got := map[string]CommunityRisk{}
	for _, cr := range rows {
		got[cr.Community.Slug] = cr
	}

	if c := got["clean"].Signals; c.Messages24h != 3 || c.Authors24h != 2 || c.DupExcess24h != 0 {
		t.Errorf("clean signals wrong: %+v", c)
	}
	b := got["bot"].Signals
	if b.Messages24h != 5 {
		t.Errorf("bot Messages24h = %d, want 5 (old message must be excluded)", b.Messages24h)
	}
	if b.Messages1h != 5 {
		t.Errorf("bot Messages1h = %d, want 5", b.Messages1h)
	}
	if b.Authors24h != 1 {
		t.Errorf("bot Authors24h = %d, want 1", b.Authors24h)
	}
	if b.DupExcess24h != 4 {
		t.Errorf("bot DupExcess24h = %d, want 4 (5 identical => 4 redundant copies)", b.DupExcess24h)
	}
}
