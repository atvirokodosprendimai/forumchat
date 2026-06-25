package templ

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// renderMsg renders one MessageView (meta shown) and returns the HTML.
func renderMsg(t *testing.T, m MsgView) string {
	t.Helper()
	var buf bytes.Buffer
	if err := MessageView(m, false, "viewer-1", "Viewer", "demo", true).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

// TestBotBubble_AsHumanHasNoBadge guards the "treat the bot as a normal citizen"
// option: an as-human agent message must render like a member (name + identity
// dot, no "AI"/"bot" badge), while a normal bot message keeps its AI badge.
func TestBotBubble_AsHumanHasNoBadge(t *testing.T) {
	base := MsgView{
		ID: "m1", Kind: MsgKindBot, AuthorName: "Helper",
		BodyHTML: "<p>hi there</p>", GenStatus: "done", CreatedAt: time.Unix(0, 0),
	}

	// Normal bot: badged.
	normal := renderMsg(t, base)
	if !strings.Contains(normal, "bot-tag") {
		t.Errorf("normal bot bubble should carry an AI/bot badge\n%s", normal)
	}

	// As-human: no badge, but the name still shows + an identity dot is rendered.
	human := base
	human.AsHuman = true
	out := renderMsg(t, human)
	for _, banned := range []string{"bot-tag", "bot-avatar", "bot-dot"} {
		if strings.Contains(out, banned) {
			t.Errorf("as-human bubble must not contain %q (it should look like a member)\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "Helper") {
		t.Errorf("as-human bubble should still show the display name\n%s", out)
	}
	if !strings.Contains(out, "author-dot") {
		t.Errorf("as-human bubble should render a member-style identity dot\n%s", out)
	}
}

// TestBotBubble_AsHumanStreamingCursor confirms the streaming cursor still shows
// while an as-human bubble is generating (the one transient tell that it's live).
func TestBotBubble_AsHumanStreamingCursor(t *testing.T) {
	out := renderMsg(t, MsgView{
		ID: "m2", Kind: MsgKindBot, AsHuman: true, AuthorName: "Helper",
		BodyHTML: "<p>typ</p>", GenStatus: "generating", CreatedAt: time.Unix(0, 0),
	})
	if !strings.Contains(out, "gen-cursor") {
		t.Errorf("as-human bubble should show the streaming cursor while generating\n%s", out)
	}
}
