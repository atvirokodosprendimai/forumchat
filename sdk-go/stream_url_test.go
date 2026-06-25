package connector

import (
	"net/url"
	"testing"
	"time"
)

// The catch-up resume point rides the stream URL as a `since` query param, so
// these tests pin how the SDK builds it: Stream/StreamURL omit it (server-cursor
// resume), StreamSince adds it as a Unix second, and the signed exp/sig stay
// intact either way.
func TestStreamURL_SinceParam(t *testing.T) {
	c := New("https://chat.example.com", testID, testSecret)

	// Default (server-cursor resume): no since param.
	if q := query(t, c.StreamURL(0)); q.Has("since") {
		t.Errorf("StreamURL must not carry since (server-cursor resume), got %q", q.Get("since"))
	}

	// Client override: since is the Unix second, alongside the signed exp+sig.
	at := time.Unix(1_700_000_000, 0)
	q := query(t, c.streamURL(0, at))
	if got := q.Get("since"); got != "1700000000" {
		t.Errorf("since = %q, want 1700000000", got)
	}
	if q.Get("sig") != streamSig(testSecret, testID, 0) {
		t.Error("streamURL must keep the bound signature when since is set")
	}
	if q.Get("exp") != "0" {
		t.Errorf("exp = %q, want 0", q.Get("exp"))
	}

	// A zero since is omitted (identical to the no-override URL).
	if q := query(t, c.streamURL(0, time.Time{})); q.Has("since") {
		t.Error("a zero since must be omitted, not sent as 0")
	}
}

// query parses the query string off a built stream URL, failing the test on a
// malformed URL so a bad build surfaces here rather than as a server 404.
func query(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u.Query()
}
