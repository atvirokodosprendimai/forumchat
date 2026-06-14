package render_test

import (
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

func TestHighlightMentions_PlainAndMe(t *testing.T) {
	t.Parallel()
	in := "hey @Alice and @bob, also @Carol-1"
	out := render.HighlightMentions(in, "Bob")
	if !strings.Contains(out, `<span class="mention">@Alice</span>`) {
		t.Errorf("missing plain mention for Alice: %s", out)
	}
	if !strings.Contains(out, `<span class="mention me">@bob</span>`) {
		t.Errorf("missing .me class for bob (viewer Bob): %s", out)
	}
	if !strings.Contains(out, `<span class="mention">@Carol-1</span>`) {
		t.Errorf("missing Carol-1 (hyphen+digit): %s", out)
	}
}

func TestHighlightMentions_EmailNotMatched(t *testing.T) {
	t.Parallel()
	in := "ping foo@bar.example with details"
	out := render.HighlightMentions(in, "")
	if strings.Contains(out, `<span class="mention"`) {
		t.Errorf("email should NOT be wrapped: %s", out)
	}
}

func TestHighlightMentions_EmptyViewer(t *testing.T) {
	t.Parallel()
	out := render.HighlightMentions("yo @alice", "")
	if !strings.Contains(out, `<span class="mention">@alice</span>`) {
		t.Errorf("expected plain mention only, got: %s", out)
	}
	if strings.Contains(out, "mention me") {
		t.Errorf("must not apply .me without viewer: %s", out)
	}
}

func TestHighlightMentions_LeadingMention(t *testing.T) {
	t.Parallel()
	out := render.HighlightMentions("@alice hello", "Alice")
	if !strings.Contains(out, `<span class="mention me">@alice</span>`) {
		t.Errorf("leading @-token should match: %s", out)
	}
}
