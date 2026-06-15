package mailbox

import (
	"strings"

	"github.com/jaytaylor/html2text"
)

// MaxIssueBodyChars caps the auto-issue body so a runaway 5MB HTML mail
// doesn't blow up the SQLite row. The truncation marker is appended so
// human readers can spot it.
const MaxIssueBodyChars = 65536

// ExtractIssueBody converts the best-available text representation of
// an email into a markdown-safe body suitable for projects.CreateIssue.
//
// If textPart is non-empty it wins (already plaintext, just normalise).
// Otherwise the HTML part is run through html2text. The result has
// markdown literals (*, _, `) escaped so the body doesn't accidentally
// re-render as bold/italics/inline-code via render.RenderMarkdown on
// the read path.
func ExtractIssueBody(textPart, htmlPart string) string {
	body := strings.TrimSpace(textPart)
	if body == "" {
		converted, err := html2text.FromString(htmlPart, html2text.Options{
			PrettyTables: false,
			OmitLinks:    false,
		})
		if err == nil {
			body = converted
		}
		body = strings.TrimSpace(body)
	}
	body = escapeMarkdownLiterals(body)
	body = collapseTrailingNewlines(body)
	if len(body) > MaxIssueBodyChars {
		body = body[:MaxIssueBodyChars] + "\n\n... [truncated]"
	}
	return body
}

func escapeMarkdownLiterals(s string) string {
	if s == "" {
		return s
	}
	r := strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"_", "\\_",
		"`", "\\`",
	)
	return r.Replace(s)
}

func collapseTrailingNewlines(s string) string {
	for strings.HasSuffix(s, "\n\n\n") {
		s = s[:len(s)-1]
	}
	return s
}
