// Package devdocs renders the first-party developer documentation (the embedded
// Markdown in package docs) into standalone HTML pages served at /dev/docs.
//
// The content is TRUSTED, first-party Markdown compiled into the binary, so the
// renderer here deliberately does NOT run output through the user-content
// sanitizer (internal/render is for untrusted user input). Two things fall out
// of that trust:
//
//   - Heading anchors survive. goldmark's auto heading IDs are kept, so the
//     table of contents can link to them; the user-content path strips those
//     ids at sanitize time.
//   - Code highlighting needs no extra stylesheet. chroma is configured with
//     inline styles (WithClasses(false)), which would be stripped by the
//     sanitizer but here are emitted straight onto each token span — so fenced
//     code blocks are syntax-highlighted with zero CSS to ship.
//
// Never route user-submitted Markdown through this package.
package devdocs

import (
	"bytes"
	"fmt"
	stdhtml "html"
	"regexp"
	"strings"
	"sync"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
)

// md is the shared, lazily-initialised Markdown engine. goldmark.Markdown is
// safe for concurrent Convert calls, so one engine serves every request.
var (
	mdOnce sync.Once
	md     goldmark.Markdown
)

// docChromaStyle is the chroma theme for code blocks. "github" is a light theme
// matching the docs' flat-light palette; emitted as inline styles so it needs
// no companion CSS (unlike internal/render, which themes via highlight.css).
const docChromaStyle = "github"

func initMarkdown() {
	md = goldmark.New(
		goldmark.WithExtensions(
			// GFM gives tables, strikethrough, autolinks, task lists — all used
			// by the docs.
			extension.GFM,
			highlighting.NewHighlighting(
				highlighting.WithStyle(docChromaStyle),
				// WithClasses(false) bakes colours into inline style attributes.
				// Trusted output is not sanitized, so they survive — no extra
				// stylesheet, no class/theme coupling.
				highlighting.WithFormatOptions(
					chromahtml.WithClasses(false),
				),
			),
		),
		// Auto heading IDs power the table-of-contents anchors. Kept because the
		// output is not sanitized.
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
}

// Render converts trusted Markdown source to an HTML fragment string. The
// caller embeds it verbatim (templ.Raw) — it is first-party content, not user
// input, so it is safe to trust.
func Render(src []byte) (string, error) {
	mdOnce.Do(initMarkdown)
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", fmt.Errorf("devdocs: render markdown: %w", err)
	}
	return buf.String(), nil
}

// TOCEntry is one heading in a doc's table of contents: its anchor id, visible
// title, and whether it is a sub-heading (h3 nested under an h2).
type TOCEntry struct {
	ID    string
	Title string
	Sub   bool
}

// headingRE captures the level, auto-generated id, and inner HTML of every h2
// and h3 in rendered output. The docs use h1 for the page title (excluded from
// the TOC) and h2/h3 for sections, which is the depth the sidebar shows.
var headingRE = regexp.MustCompile(`(?s)<h([23]) id="([^"]+)">(.*?)</h[23]>`)

// innerTagRE strips any inline markup (e.g. <code>) from a heading's inner HTML
// so the TOC label is plain text.
var innerTagRE = regexp.MustCompile(`<[^>]+>`)

// TableOfContents extracts the h2/h3 outline from rendered doc HTML. It reads
// the ids goldmark already assigned, so the returned anchors match the headings
// in the page exactly. Returns nil when the doc has no sectioning headings.
func TableOfContents(rendered string) []TOCEntry {
	matches := headingRE.FindAllStringSubmatch(rendered, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]TOCEntry, 0, len(matches))
	for _, m := range matches {
		title := strings.TrimSpace(innerTagRE.ReplaceAllString(m[3], ""))
		out = append(out, TOCEntry{
			ID:    m[2],
			Title: stdhtml.UnescapeString(title),
			Sub:   m[1] == "3",
		})
	}
	return out
}
