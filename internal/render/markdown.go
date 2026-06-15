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

// existingImgAnchorRE matches any `<a href="/uploads/..."> <img
// src="/uploads/..."> </a>` shape — covers both our own
// WrapUploadImages output AND the chat/forum `[![](u)](u)` markdown
// pattern that goldmark emits. We strip these before re-wrapping so
// the final output is always exactly one anchor per img (no nesting,
// which browsers collapse to an empty outer anchor).
var existingImgAnchorRE = regexp.MustCompile(`<a[^>]*?href="/uploads/[^"]+"[^>]*?>\s*(<img [^>]*?src="/uploads/[^"]+"[^>]*?>)\s*</a>`)

// uploadsAnchorRE matches anchors pointing at our signed-upload URLs so we
// can force target="_blank" rel="noopener" on them after sanitization. We
// run this AFTER bluemonday so the added attributes are trusted output.
var uploadsAnchorRE = regexp.MustCompile(`<a (href="/uploads/)`)

// uploadSchemeRE matches src/href attributes pointing at the
// `upload://<uploadID>` placeholder scheme. The mailbox CID rewriter
// writes these into auto-issue bodies so the view layer can resolve
// each <uploadID> to a viewer-specific signed URL at display time.
// Two groups: tag attribute name (src or href) + the upload id.
var uploadSchemeRE = regexp.MustCompile(`(src|href)="upload://([a-zA-Z0-9_-]+)"`)

// ResolveUploadURLs replaces every `upload://<id>` reference in an
// HTML fragment with a viewer-specific signed URL produced by signer.
// Used by handlers that render bodies stored with the upload://
// placeholder scheme (auto-issues with inline cid: images). signer
// returning "" for an unknown id leaves the attribute as-is so a
// missing inline asset doesn't break the surrounding markup.
func ResolveUploadURLs(s string, signer func(uploadID string) string) string {
	if s == "" || signer == nil {
		return s
	}
	return uploadSchemeRE.ReplaceAllStringFunc(s, func(match string) string {
		groups := uploadSchemeRE.FindStringSubmatch(match)
		if len(groups) != 3 {
			return match
		}
		signed := signer(groups[2])
		if signed == "" {
			return match
		}
		return groups[1] + `="` + signed + `"`
	})
}

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
	// "upload" is the placeholder scheme written by the mailbox CID
	// rewriter (mailbox.RewriteCIDImages → ![inline](upload://<uploadID>)).
	// ResolveUploadURLs swaps each occurrence to a signed `/uploads/...`
	// URL at view time. Without this entry bluemonday strips the
	// upload://… src out at sanitize time, leaving the user with <img
	// alt="inline"/> and no source.
	p.AllowURLSchemes("http", "https", "mailto", "upload")
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
	return out, nil
}

// mentionTokenRE matches an `@name` token that's NOT preceded by an
// identifier-like char (so `email@host` won't match) and whose name is
// 1..32 chars of the same alphabet allowed by the typeahead popup.
var mentionTokenRE = regexp.MustCompile(`(^|[^A-Za-z0-9_\-])@([A-Za-z0-9_\-]{1,32})`)

// HighlightMentions wraps every `@token` in the rendered chat HTML with
// a `<span class="mention">` so CSS can style mentions; tokens that
// match the viewer's display name (case-insensitive) get an extra `.me`
// class so the viewer's own pings stand out. Runs at display time
// (viewer-aware), so two viewers reading the same message see different
// classes.
//
// Caveat: the regex doesn't try to skip mentions inside <code>/<pre>;
// in practice goldmark escapes `@` outside fenced code anyway, and a
// stray highlight inside a code block is purely cosmetic.
func HighlightMentions(htmlBody, viewerDisplayName string) string {
	if htmlBody == "" {
		return htmlBody
	}
	viewer := strings.ToLower(strings.TrimSpace(viewerDisplayName))
	return mentionTokenRE.ReplaceAllStringFunc(htmlBody, func(m string) string {
		sub := mentionTokenRE.FindStringSubmatch(m)
		prefix, name := sub[1], sub[2]
		cls := "mention"
		if viewer != "" && strings.ToLower(name) == viewer {
			cls = "mention me"
		}
		return prefix + `<span class="` + cls + `">@` + name + `</span>`
	})
}

// WrapUploadImages wraps every `<img src="/uploads/...">` in an anchor
// that opens the original in a new tab. Idempotent: it first strips
// any existing wrap then re-applies one, so running twice (or on
// content already processed by a prior render pass) yields the same
// result.
//
// Run at DISPLAY time so already-stored bodies pick up the wrap
// without a migration. New writes don't get wrapped at write time —
// keeps the DB free of presentation HTML.
func WrapUploadImages(s string) string {
	// Step 1: strip any existing anchor-around-img — covers both prior
	// WrapUploadImages output AND chat/forum's [![](u)](u) markdown
	// pattern that already emits a link-wrapped img.
	s = existingImgAnchorRE.ReplaceAllString(s, `${1}`)
	// Step 2: uniformly wrap every img in our own anchor. Brace the
	// group refs (${1}, ${2}, ${3}) so Go's regexp template doesn't
	// greedy-parse `$1src` as a group named "1src" (undefined → empty,
	// which silently drops the `src=` literal and produces a broken
	// <img> tag).
	return uploadsImageRE.ReplaceAllString(s,
		`<a target="_blank" rel="noopener" href="${2}" class="upload-img-link"><img ${1}src="${2}"${3}></a>`)
}
