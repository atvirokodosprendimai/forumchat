package render

import (
	"regexp"
	"strings"
)

// imageMarkdownRE matches a leading markdown image, optionally wrapped in a
// link: `![alt](src)` or `[![alt](src)](href)`.
var imageMarkdownRE = regexp.MustCompile(`^\[?!\[[^\]]*\]\([^)]*\)\]?(?:\([^)]*\))?`)

// stripLeadingImage returns the body with any leading markdown image syntax
// removed and trimmed. When the result is empty AND the input wasn't, the
// second return is true so callers can substitute a placeholder.
func stripLeadingImage(s string) (string, bool) {
	stripped := strings.TrimSpace(imageMarkdownRE.ReplaceAllString(s, ""))
	return stripped, stripped == "" && strings.TrimSpace(s) != ""
}

// AutoTitle derives a friendly single-line title from a markdown body.
// Image-only messages collapse to "(image)"; image-prefixed bodies have the
// image syntax stripped before truncation. Used by bookmark and todo
// creation flows.
func AutoTitle(md string) string {
	if i := strings.IndexAny(md, "\r\n"); i >= 0 {
		md = md[:i]
	}
	md = strings.TrimSpace(md)
	stripped, wasImage := stripLeadingImage(md)
	if wasImage {
		return "(image)"
	}
	md = stripped
	if len(md) > 120 {
		md = md[:120] + "…"
	}
	return md
}
