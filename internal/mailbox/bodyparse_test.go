package mailbox

import (
	"strings"
	"testing"
)

func TestExtractIssueBody_PlainWins(t *testing.T) {
	got := ExtractIssueBody("hello *plain* text", "<p>hello <b>html</b> text</p>")
	if !strings.Contains(got, "hello \\*plain\\* text") {
		t.Fatalf("plain should win and be escaped, got %q", got)
	}
	if strings.Contains(got, "<p>") || strings.Contains(got, "<b>") {
		t.Fatalf("HTML should not appear when plain available, got %q", got)
	}
}

func TestExtractIssueBody_HTMLFallback(t *testing.T) {
	got := ExtractIssueBody("", "<p>Hello <b>world</b></p>")
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "world") {
		t.Fatalf("html should be converted to text, got %q", got)
	}
	if strings.Contains(got, "<p>") {
		t.Fatalf("html tags should be stripped, got %q", got)
	}
}

func TestExtractIssueBody_TruncatesAtCap(t *testing.T) {
	big := strings.Repeat("a", MaxIssueBodyChars+1000)
	got := ExtractIssueBody(big, "")
	if !strings.Contains(got, "... [truncated]") {
		t.Fatalf("expected truncation marker, got tail %q", got[len(got)-40:])
	}
	if len(got) > MaxIssueBodyChars+50 {
		t.Fatalf("body too long after truncate: %d", len(got))
	}
}

func TestEscapeMarkdownLiterals(t *testing.T) {
	if got := escapeMarkdownLiterals("a*b_c`d"); got != "a\\*b\\_c\\`d" {
		t.Fatalf("got %q", got)
	}
	if got := escapeMarkdownLiterals(""); got != "" {
		t.Fatalf("empty should pass through")
	}
}
