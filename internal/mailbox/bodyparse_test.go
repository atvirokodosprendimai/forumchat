package mailbox

import (
	"strings"
	"testing"
)

func TestDecodeTextBody_EmptyEncodingSniffBase64(t *testing.T) {
	// Body wire bytes are base64 but the BODYSTRUCTURE didn't report a
	// transfer encoding. decodeTextBody must sniff and decode anyway,
	// otherwise prod stores the literal base64 string (the bug that
	// shipped in commit 8e928a0's repair-tool drawer).
	raw := []byte("R2FsdXRpbsSXIHZhcnRvdG9qxbMgcHJvZ3JhbcSXbMSXIHZhZGluYXNpIFBhcmtUaW1l")
	got := decodeTextBody(raw, "", "utf-8")
	if !strings.Contains(got, "Galutin") || !strings.Contains(got, "ParkTime") {
		t.Fatalf("expected decoded Lithuanian text, got %q", got)
	}
}

func TestDecodeTextBody_PlainPassthrough(t *testing.T) {
	// Pure-ascii body with no encoding declared should NOT be mis-decoded
	// (false-positive base64 sniff). Even though "Hello World" has no
	// base64 alphabet trespasses, it's mod-4 mid-trim. Sniff guards on
	// length + alphabet so plain text passes through.
	got := decodeTextBody([]byte("Hello there"), "", "utf-8")
	if got != "Hello there" {
		t.Fatalf("plain body should pass through, got %q", got)
	}
}

func TestDecodeTextBody_EmptyEncodingSniffQuotedPrintable(t *testing.T) {
	// Body wire bytes are quoted-printable (=XX hex escapes) but the
	// BODYSTRUCTURE didn't report a transfer encoding. decodeTextBody
	// must sniff QP and decode; otherwise the user sees =D3 etc.
	raw := []byte("Hello =D3 world =0A second line")
	got := decodeTextBody(raw, "", "iso-8859-1")
	if strings.Contains(got, "=D3") {
		t.Fatalf("QP escape not decoded: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "second line") {
		t.Fatalf("plain text lost: %q", got)
	}
}

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
