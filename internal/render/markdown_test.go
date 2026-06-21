package render_test

import (
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

func TestRenderMarkdown_UploadSchemePreserved(t *testing.T) {
	t.Parallel()
	// Markdown image with the upload:// placeholder scheme written by
	// the mailbox CID rewriter must survive bluemonday sanitize so the
	// view-time ResolveUploadURLs has an attribute to swap.
	in := "Here is the inline shot: ![inline](upload://abc-123)"
	out, err := render.RenderMarkdown(in)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, `src="upload://abc-123"`) {
		t.Fatalf("upload:// scheme stripped, got: %s", out)
	}
}

func TestResolveUploadURLs_SwapsViaSigner(t *testing.T) {
	t.Parallel()
	in := `<img alt="inline" src="upload://abc-123"/>`
	out := render.ResolveUploadURLs(in, func(id string) string {
		return "/uploads/sha?sig=xxx"
	})
	if !strings.Contains(out, `src="/uploads/sha?sig=xxx"`) {
		t.Fatalf("signer output not applied: %s", out)
	}
	if strings.Contains(out, "upload://") {
		t.Fatalf("placeholder not removed: %s", out)
	}
}

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

func TestLinkNewTab(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "external link gets target blank",
			in:   `<a href="https://example.com" rel="nofollow noreferrer">example</a>`,
			want: `<a target="_blank" href="https://example.com" rel="nofollow noreferrer">example</a>`,
		},
		{
			name: "upload anchor already has target, left alone",
			in:   `<a target="_blank" rel="noopener" href="/uploads/x">img</a>`,
			want: `<a target="_blank" rel="noopener" href="/uploads/x">img</a>`,
		},
		{
			name: "relative link untouched",
			in:   `<a href="/c/slug/forum">forum</a>`,
			want: `<a href="/c/slug/forum">forum</a>`,
		},
		{
			name: "idempotent on its own output",
			in:   `<a target="_blank" href="https://example.com">x</a>`,
			want: `<a target="_blank" href="https://example.com">x</a>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := render.LinkNewTab(tc.in); got != tc.want {
				t.Errorf("LinkNewTab(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Full pipeline: a markdown external link rendered then passed through the
// agent-bubble display chain must end up target="_blank".
func TestLinkNewTab_FromRenderedMarkdown(t *testing.T) {
	t.Parallel()
	html, err := render.RenderMarkdown("see [the site](https://example.com)")
	if err != nil {
		t.Fatal(err)
	}
	out := render.LinkNewTab(render.WrapUploadImages(html))
	if !strings.Contains(out, `target="_blank"`) {
		t.Fatalf("expected target=\"_blank\" in %q", out)
	}
	if !strings.Contains(out, `href="https://example.com"`) {
		t.Fatalf("expected href preserved in %q", out)
	}
}
