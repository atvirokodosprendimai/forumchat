package templ

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestAgentRateLimitNotice renders the throttle partial that PostSend /
// PostReply morph into the composer's notice slot. Guards the deliverable: an
// error card with the right slot id, the rounded retry seconds, a self-dismiss
// hook, and a manual dismiss button.
func TestAgentRateLimitNotice(t *testing.T) {
	var buf bytes.Buffer
	// 4.2s should round UP to 5.
	if err := AgentRateLimitNotice("chat-agent-notice", 4200*time.Millisecond).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`id="chat-agent-notice"`, // stable slot id PostSend targets
		"agent-rate-notice",      // the card
		"Rate limit reached",     // the error message
		"5s",                     // ceil(4.2s)
		"setTimeout",             // self-dismiss
		"Dismiss",                // manual close affordance
	} {
		if !strings.Contains(out, want) {
			t.Errorf("notice missing %q\n--- got ---\n%s", want, out)
		}
	}
}

// TestRateLimitSecs covers the ceil-with-floor-of-1 rounding.
func TestRateLimitSecs(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "1"},                       // floor at 1
		{500 * time.Millisecond, "1"},  // sub-second rounds up to 1
		{time.Second, "1"},             // exact
		{1500 * time.Millisecond, "2"}, // ceil
		{60 * time.Second, "60"},
	}
	for _, c := range cases {
		if got := rateLimitSecs(c.in); got != c.want {
			t.Errorf("rateLimitSecs(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
