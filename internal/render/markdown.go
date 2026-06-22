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

// ytLinkRE matches a sanitized anchor so EmbedYouTube can inspect its href.
// Anchors don't nest, so the non-greedy body is safe. Runs at DISPLAY time on
// already-sanitized HTML (after bluemonday), so the facade it injects is trusted
// output — same contract as WrapUploadImages / LinkNewTab.
var ytLinkRE = regexp.MustCompile(`<a\b[^>]*\bhref="([^"]+)"[^>]*>.*?</a>`)

// ytIDRE extracts the 11-char video id from the common YouTube URL shapes
// (watch?v=, youtu.be/, /shorts/, /embed/, /v/). The href is HTML-escaped
// (& → &amp;) but the id charset stops before any '&', so that's irrelevant.
var ytIDRE = regexp.MustCompile(`(?:youtu\.be/|youtube(?:-nocookie)?\.com/(?:embed/|shorts/|v/|watch\?[^"]*\bv=))([A-Za-z0-9_-]{11})`)

// EmbedYouTube replaces every anchor whose href is a YouTube link with a
// lightweight click-to-play facade (thumbnail + play badge). The facade is a
// plain <img>, so the chat fat-morph re-renders it cheaply; clicking opens the
// player in the global lightbox overlay (web/templ/layout.templ) which lives
// outside #messages and so survives a morph (a live in-bubble iframe would
// reload on every chat event). The original link stays as the href for a no-JS
// fallback. Non-YouTube anchors pass through untouched.
func EmbedYouTube(s string) string {
	if s == "" {
		return s
	}
	return ytLinkRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := ytLinkRE.FindStringSubmatch(m)
		ids := ytIDRE.FindStringSubmatch(sub[1])
		if ids == nil {
			return m
		}
		return ytFacade(ids[1], sub[1])
	})
}

// ytFacade is the click-to-play thumbnail. The thumbnail is a CSS
// background-image on the anchor (not an <img>) so ambient message-body image
// rules (e.g. `.body img { height:100px }`) can't distort it. id is
// [A-Za-z0-9_-]{11} (safe inside the inline style URL and the single-quoted
// Datastar expression — no escaping needed); href is the already-escaped
// original URL, kept as a no-JS fallback that opens YouTube.
func ytFacade(id, href string) string {
	return `<a class="yt-embed" href="` + href + `" target="_blank" rel="noopener nofollow"` +
		` style="background-image:url('https://i.ytimg.com/vi/` + id + `/hqdefault.jpg')"` +
		` data-on:click="evt.preventDefault();$_yt_id='` + id + `';$_yt_open=true"` +
		` aria-label="Play YouTube video">` +
		`<span class="yt-embed-play" aria-hidden="true"></span></a>`
}

// RichHTML applies the standard DISPLAY-time enrichments to already-rendered
// (sanitized) body HTML: upload-image anchors, then YouTube embeds. It is the
// single chokepoint every body-render call site uses so the enrichment set stays
// consistent across chat, forum, projects, etc. Mention highlighting and new-tab
// rewriting stay caller-specific wrappers around this (they need extra args /
// only apply on some surfaces).
func RichHTML(s string) string {
	return DownloadableCode(EmbedYouTube(WrapUploadImages(s)))
}

// codeBlockRE matches a full fenced code block as emitted by goldmark +
// bluemonday: `<pre><code[ class="language-XXX"]>…</code></pre>`. goldmark
// escapes `<` to `&lt;` inside code, so the body never contains a literal
// `</code>` and the non-greedy capture is unambiguous. Group 1 is the language
// token (possibly absent); the body is left in place via the full match.
var codeBlockRE = regexp.MustCompile(`(?s)<pre><code(?: class="language-([\w.+#-]+)")?>.*?</code></pre>`)

// codeExt maps a fenced-block language token to a download filename extension
// and MIME type. Unknown or absent languages fall back to a plain .txt file.
func codeExt(lang string) (ext, mime string) {
	switch strings.ToLower(lang) {
	case "html", "htm", "xhtml":
		return "html", "text/html"
	case "js", "javascript", "mjs":
		return "js", "text/javascript"
	case "ts", "typescript":
		return "ts", "text/plain"
	case "jsx":
		return "jsx", "text/plain"
	case "tsx":
		return "tsx", "text/plain"
	case "go", "golang":
		return "go", "text/plain"
	case "py", "python":
		return "py", "text/x-python"
	case "css":
		return "css", "text/css"
	case "json":
		return "json", "application/json"
	case "xml":
		return "xml", "application/xml"
	case "yaml", "yml":
		return "yml", "text/yaml"
	case "sql":
		return "sql", "application/sql"
	case "sh", "bash", "shell", "zsh":
		return "sh", "text/x-shellscript"
	case "md", "markdown":
		return "md", "text/markdown"
	case "toml":
		return "toml", "text/plain"
	case "rs", "rust":
		return "rs", "text/plain"
	case "java":
		return "java", "text/x-java"
	case "c":
		return "c", "text/x-c"
	case "cpp", "c++", "cc":
		return "cpp", "text/x-c"
	default:
		return "txt", "text/plain"
	}
}

// DownloadableCode appends a "download" affordance to the end of every fenced
// code block in already-sanitized body HTML. It runs at DISPLAY time (after
// bluemonday), so the injected <figure>/<button> and its Datastar
// data-on:click are trusted output — same contract as EmbedYouTube /
// WrapUploadImages. The button reads the adjacent code's textContent and saves
// it as a file client-side (web/static/codeblock.js): no server round-trip, and
// the affordance survives the chat fat-morph because it is part of the morphed
// fragment.
func DownloadableCode(s string) string {
	if s == "" || !strings.Contains(s, "<pre><code") {
		return s
	}
	return codeBlockRE.ReplaceAllStringFunc(s, func(m string) string {
		lang := ""
		if sub := codeBlockRE.FindStringSubmatch(m); len(sub) == 2 {
			lang = sub[1]
		}
		ext, mime := codeExt(lang)
		bar := ""
		// HTML blocks also get a "Preview" that runs the code in the global
		// sandboxed iframe overlay (web/templ/layout.templ). fcPreviewCode reads
		// the code textContent; $_html_open shows the overlay.
		if ext == "html" {
			bar += `<button type="button" class="codeblock-prev"` +
				` data-on:click="window.fcPreviewCode(el);$_html_open=true">👁 Preview</button>`
		}
		bar += `<button type="button" class="codeblock-dl" data-ext="` + ext + `" data-mime="` + mime + `"` +
			` data-on:click="window.fcDownloadCode(el)">⬇ Download .` + ext + `</button>`
		return `<figure class="codeblock">` + m +
			`<figcaption class="codeblock-bar">` + bar + `</figcaption></figure>`
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

// anchorOpenRE matches an opening `<a …>` tag so we can post-process its
// attributes at display time.
var anchorOpenRE = regexp.MustCompile(`<a\s[^>]*>`)

// LinkNewTab forces external (http/https) links to open in a new tab by
// adding target="_blank" to any anchor that doesn't already declare a target.
// Runs at DISPLAY time on already-sanitized (bluemonday) HTML, so the added
// attribute is trusted output. Upload anchors already carry target="_blank"
// from RenderMarkdown / WrapUploadImages, so the "no existing target" guard
// keeps this idempotent and never duplicates the attribute. Security: external
// links already carry rel="noreferrer" (RequireNoReferrerOnLinks), which
// blocks window.opener access, so no extra rel is needed.
func LinkNewTab(s string) string {
	return anchorOpenRE.ReplaceAllStringFunc(s, func(tag string) string {
		if strings.Contains(tag, "target=") || !strings.Contains(tag, `href="http`) {
			return tag
		}
		return `<a target="_blank"` + tag[len("<a"):]
	})
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
