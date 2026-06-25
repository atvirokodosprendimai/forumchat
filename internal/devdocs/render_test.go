package devdocs

import (
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/docs"
)

// TestRenderHeadingsAndTOC checks the load-bearing property of the trusted
// renderer: auto heading ids survive (they are NOT sanitized away here, unlike
// internal/render), and TableOfContents extracts anchors that match them — so
// the on-this-page TOC actually links to real headings.
func TestRenderHeadingsAndTOC(t *testing.T) {
	src := []byte("# Title\n\n## First section\n\nbody\n\n### A sub\n\nmore\n\n## Second section\n")
	html, err := Render(src)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(html, `<h2 id="first-section">`) {
		t.Errorf("expected auto heading id in output, got:\n%s", html)
	}

	toc := TableOfContents(html)
	// h1 is the page title and excluded; we expect two h2 + one h3.
	if len(toc) != 3 {
		t.Fatalf("expected 3 TOC entries, got %d: %+v", len(toc), toc)
	}
	if toc[0].ID != "first-section" || toc[0].Title != "First section" || toc[0].Sub {
		t.Errorf("unexpected first TOC entry: %+v", toc[0])
	}
	if !toc[1].Sub || toc[1].Title != "A sub" {
		t.Errorf("expected h3 sub entry, got: %+v", toc[1])
	}
	// Every TOC anchor must resolve to an id present in the HTML.
	for _, e := range toc {
		if !strings.Contains(html, `id="`+e.ID+`"`) {
			t.Errorf("TOC anchor %q has no matching heading id in HTML", e.ID)
		}
	}
}

// TestRenderHighlightsCode confirms fenced code is highlighted with inline
// styles (chroma WithClasses(false)), which is what lets code blocks render
// coloured on the standalone page with no companion stylesheet.
func TestRenderHighlightsCode(t *testing.T) {
	html, err := Render([]byte("```go\npackage main\n```\n"))
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(html, "style=") {
		t.Errorf("expected inline chroma styles in highlighted code, got:\n%s", html)
	}
}

// TestRenderEmbeddedDocs renders every shipped doc end-to-end so a malformed
// doc (or a renderer regression) fails the build rather than 500ing in prod.
func TestRenderEmbeddedDocs(t *testing.T) {
	for _, d := range docs.List() {
		_, src, ok := docs.Get(d.Slug)
		if !ok {
			t.Fatalf("doc %q missing", d.Slug)
		}
		if _, err := Render(src); err != nil {
			t.Errorf("Render(%q): %v", d.Slug, err)
		}
	}
}
