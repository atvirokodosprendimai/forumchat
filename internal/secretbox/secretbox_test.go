package secretbox

import "testing"

func TestBox_RoundTrip(t *testing.T) {
	b, err := New("0123456789abcdef0123456789abcdef") // 32 bytes
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !b.Enabled() {
		t.Fatal("32-byte key must enable encryption")
	}
	const secret = "qdrant-api-key-xyz"
	sealed, err := b.Seal(secret)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed == secret {
		t.Fatal("sealed value must not equal plaintext")
	}
	if got, err := b.Open(sealed); err != nil || got != secret {
		t.Fatalf("Open: got %q err %v, want %q", got, err, secret)
	}
}

func TestBox_Passthrough(t *testing.T) {
	b, _ := New("") // dev: no key
	if b.Enabled() {
		t.Fatal("empty key must NOT enable encryption")
	}
	sealed, _ := b.Seal("hello")
	if sealed != "plain:hello" {
		t.Fatalf("passthrough seal = %q, want plain:hello", sealed)
	}
	if got, _ := b.Open(sealed); got != "hello" {
		t.Fatalf("passthrough open = %q, want hello", got)
	}
	// Bare legacy plaintext (no prefix) reads back as-is.
	if got, _ := b.Open("legacy"); got != "legacy" {
		t.Fatalf("legacy open = %q, want legacy", got)
	}
	// Empty stays empty both ways.
	if s, _ := b.Seal(""); s != "" {
		t.Fatalf("seal empty = %q, want empty", s)
	}
}

func TestNew_BadKeyLen(t *testing.T) {
	if _, err := New("too-short"); err == nil {
		t.Fatal("non-32-byte key must error")
	}
}
