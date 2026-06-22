package render_test

import (
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/render"
)

// render runs the real write pipeline so the test exercises DownloadableCode
// against actual goldmark+bluemonday output, not a hand-crafted <pre>.
func renderMD(t *testing.T, md string) string {
	t.Helper()
	html, err := render.RenderMarkdown(md)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return html
}

func TestDownloadableCode_HTMLBlock(t *testing.T) {
	t.Parallel()
	out := render.DownloadableCode(renderMD(t, "```html\n<div>hi</div>\n```"))

	for _, want := range []string{
		`class="codeblock"`,
		`<pre><code class="language-html">`, // original block preserved
		`data-ext="html"`,
		`data-mime="text/html"`,
		`data-on:click="window.fcDownloadCode(el)"`,
		`Download .html`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestDownloadableCode_NoLanguageFallsBackToTxt(t *testing.T) {
	t.Parallel()
	out := render.DownloadableCode(renderMD(t, "```\nplain text\n```"))
	if !strings.Contains(out, `data-ext="txt"`) || !strings.Contains(out, `data-mime="text/plain"`) {
		t.Errorf("want txt/text-plain fallback, got:\n%s", out)
	}
}

func TestDownloadableCode_NonCodeUntouched(t *testing.T) {
	t.Parallel()
	in := renderMD(t, "just a **paragraph** with `inline` code, no fence")
	if got := render.DownloadableCode(in); got != in {
		t.Errorf("non-fenced HTML changed:\nin:  %s\nout: %s", in, got)
	}
}

func TestRichHTML_WiresDownloadButton(t *testing.T) {
	t.Parallel()
	out := render.RichHTML(renderMD(t, "```go\nfmt.Println(\"hi\")\n```"))
	if !strings.Contains(out, `data-ext="go"`) || !strings.Contains(out, "class=\"codeblock\"") {
		t.Errorf("RichHTML did not wire DownloadableCode:\n%s", out)
	}
}
