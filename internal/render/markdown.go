package render

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// uploadsImageRE matches `<img src="/uploads/..."` so we can wrap each
// image in an anchor that opens the original full-size file in a new
// tab. Without this, images render as thumbnails with no way to view
// the full-size original. The bluemonday sanitizer drops onClick
// handlers so a wrapping <a> is the only viable affordance.
var uploadsImageRE = regexp.MustCompile(`<img ([^>]*?)src="(/uploads/[^"]+)"([^>]*)>`)

// uploadsAnchorRE matches anchors pointing at our signed-upload URLs so we
// can force target="_blank" rel="noopener" on them after sanitization. We
// run this AFTER bluemonday so the added attributes are trusted output.
var uploadsAnchorRE = regexp.MustCompile(`<a (href="/uploads/)`)

// userMarkdownLinkRE matches `[label](href)` — the first capture avoids the
// image form `![…](…)` (Go regex has no lookbehind).
var userMarkdownLinkRE = regexp.MustCompile(`(^|[^!\\])\[([^\]]+)\]\(([^)]+)\)`)

// sanitizeUserMarkdown enforces "no hidden URLs" Discord-style: every
// `[label](href)` whose label is not the href is rewritten so the visible
// text equals the destination. Images stay as images (Discord allows
// inline embeds from any host). Image-wrapped-in-link `[![..](..)](..)`
// from the paste pipeline is preserved by skipping any link whose label
// starts with `!` (markdown image).
func sanitizeUserMarkdown(s string) string {
	return userMarkdownLinkRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := userMarkdownLinkRE.FindStringSubmatch(m)
		prefix, label, href := sub[1], sub[2], sub[3]
		if strings.HasPrefix(label, "!") {
			return m
		}
		if strings.TrimSpace(label) == strings.TrimSpace(href) {
			return m
		}
		return prefix + " " + href + " "
	})
}

var (
	mdOnce sync.Once
	md     goldmark.Markdown
	policy *bluemonday.Policy
)

func initMarkdown() {
	md = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
		goldmark.WithRendererOptions(html.WithHardWraps(), html.WithXHTML()),
	)
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").OnElements("code", "pre", "span")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AllowURLSchemes("http", "https", "mailto")
	policy = p
}

func RenderMarkdown(src string) (string, error) {
	mdOnce.Do(initMarkdown)
	src = sanitizeUserMarkdown(src)
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("markdown convert: %w", err)
	}
	out := policy.Sanitize(buf.String())
	out = uploadsAnchorRE.ReplaceAllString(out, `<a target="_blank" rel="noopener" $1`)
	out = uploadsImageRE.ReplaceAllString(out,
		`<a target="_blank" rel="noopener" href="$2" class="upload-img-link"><img $1src="$2"$3></a>`)
	return out, nil
}
