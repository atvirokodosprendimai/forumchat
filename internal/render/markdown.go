package render

import (
	"bytes"
	"fmt"
	"regexp"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// uploadsAnchorRE matches anchors pointing at our signed-upload URLs so we
// can force target="_blank" rel="noopener" on them after sanitization. We
// run this AFTER bluemonday so the added attributes are trusted output.
var uploadsAnchorRE = regexp.MustCompile(`<a (href="/uploads/)`)

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
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return "", fmt.Errorf("markdown convert: %w", err)
	}
	out := policy.Sanitize(buf.String())
	out = uploadsAnchorRE.ReplaceAllString(out, `<a target="_blank" rel="noopener" $1`)
	return out, nil
}
