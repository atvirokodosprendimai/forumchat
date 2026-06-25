package connectors

import (
	"net/url"
	"strconv"
	"testing"
	"time"
)

// itoa formats a unix second for a query string (kept tiny + local).
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// TestResumeWatermark pins the resume policy: the server cursor is the default
// (stateless worker), ?live=1 / ?since= override it, and the maxCatchupWindow
// clamp bounds (and flags) an over-old request. now is fixed so the window math
// is deterministic.
func TestResumeWatermark(t *testing.T) {
	now := time.Unix(1_900_000_000, 0) // fixed "now"
	windowStart := now.Add(-maxCatchupWindow)
	ptr := func(t time.Time) *time.Time { return &t }
	q := func(raw string) url.Values {
		v, _ := url.ParseQuery(raw)
		return v
	}

	cases := []struct {
		name        string
		conn        Connector
		query       string
		wantFrom    time.Time
		wantCatchUp bool
		wantTrunc   bool
	}{
		{
			name:        "no cursor, no params → live-only from now",
			conn:        Connector{},
			query:       "",
			wantFrom:    now,
			wantCatchUp: false,
		},
		{
			name:        "live=1 overrides a stored cursor → live-only",
			conn:        Connector{CursorAt: ptr(now.Add(-time.Hour))},
			query:       "live=1",
			wantFrom:    now,
			wantCatchUp: false,
		},
		{
			name:        "stored cursor resumes (stateless-worker default)",
			conn:        Connector{CursorAt: ptr(now.Add(-30 * time.Minute))},
			query:       "",
			wantFrom:    now.Add(-30 * time.Minute),
			wantCatchUp: true,
		},
		{
			name:        "since overrides the cursor",
			conn:        Connector{CursorAt: ptr(now.Add(-time.Hour))},
			query:       "since=" + itoa(now.Add(-10*time.Minute).Unix()),
			wantFrom:    now.Add(-10 * time.Minute),
			wantCatchUp: true,
		},
		{
			name:        "reset cursor (0/epoch) → replay the whole window, truncated",
			conn:        Connector{CursorAt: ptr(time.Unix(0, 0))},
			query:       "",
			wantFrom:    windowStart,
			wantCatchUp: true,
			wantTrunc:   true,
		},
		{
			name:        "since older than the window is clamped + truncated",
			conn:        Connector{},
			query:       "since=" + itoa(now.Add(-48*time.Hour).Unix()),
			wantFrom:    windowStart,
			wantCatchUp: true,
			wantTrunc:   true,
		},
		{
			name:        "future since snaps to now (no catch-up)",
			conn:        Connector{},
			query:       "since=" + itoa(now.Add(time.Hour).Unix()),
			wantFrom:    now,
			wantCatchUp: false,
		},
		{
			name:        "malformed since is ignored → falls back to the cursor",
			conn:        Connector{CursorAt: ptr(now.Add(-5 * time.Minute))},
			query:       "since=notanumber",
			wantFrom:    now.Add(-5 * time.Minute),
			wantCatchUp: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, catchUp, trunc := resumeWatermark(tc.conn, q(tc.query), now)
			if !from.Equal(tc.wantFrom) {
				t.Errorf("resumeFrom = %v, want %v", from.Unix(), tc.wantFrom.Unix())
			}
			if catchUp != tc.wantCatchUp {
				t.Errorf("catchUp = %v, want %v", catchUp, tc.wantCatchUp)
			}
			if trunc != tc.wantTrunc {
				t.Errorf("truncated = %v, want %v", trunc, tc.wantTrunc)
			}
		})
	}
}
