package rag

import (
	"strings"
	"testing"
)

func TestChunkEmpty(t *testing.T) {
	if got := chunk("", "", ChunkConfig{BodyTokens: 100, Overlap: 10}); got != nil {
		t.Fatalf("empty input: want nil, got %v", got)
	}
	if got := chunk("   ", "  \n ", ChunkConfig{BodyTokens: 100, Overlap: 10}); got != nil {
		t.Fatalf("whitespace input: want nil, got %v", got)
	}
}

func TestChunkShortSingle(t *testing.T) {
	got := chunk("Title", "a short body", ChunkConfig{BodyTokens: 2800, Overlap: 400})
	if len(got) != 1 {
		t.Fatalf("short body should be one chunk, got %d", len(got))
	}
	if !strings.Contains(got[0], "Title") || !strings.Contains(got[0], "short body") {
		t.Fatalf("chunk should include title and body: %q", got[0])
	}
}

func TestChunkSlidingWindow(t *testing.T) {
	// 1000 one-token words, body window 100, overlap 20.
	words := make([]string, 1000)
	for i := range words {
		words[i] = "w"
	}
	body := strings.Join(words, " ")
	cfg := ChunkConfig{BodyTokens: 100, Overlap: 20}
	chunks := chunk("", body, cfg)

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	// Each chunk must stay within the budget (body + 2*overlap) plus slack for
	// the boundary word; assert a hard ceiling well under bge-m3's 4k.
	maxWords := cfg.BodyTokens + 2*cfg.Overlap + 1
	for i, c := range chunks {
		n := len(strings.Fields(c))
		if n > maxWords {
			t.Fatalf("chunk %d has %d words, exceeds budget %d", i, n, maxWords)
		}
		if n == 0 {
			t.Fatalf("chunk %d is empty", i)
		}
	}
	// Primary windows advance by BodyTokens, so ~10 chunks for 1000/100.
	if len(chunks) < 9 || len(chunks) > 11 {
		t.Fatalf("expected ~10 chunks for 1000 words / window 100, got %d", len(chunks))
	}
}

func TestChunkOversizedWordProgresses(t *testing.T) {
	// A single huge word must not loop forever — it forces progress.
	huge := strings.Repeat("x", 5000) // ~1250 estimated tokens, > body window
	got := chunk("", huge+" tail", ChunkConfig{BodyTokens: 100, Overlap: 10})
	if len(got) == 0 {
		t.Fatal("oversized word produced no chunks")
	}
}

func TestEstTokens(t *testing.T) {
	cases := map[string]int{"a": 1, "abcd": 1, "abcde": 2, "abcdefgh": 2, "abcdefghi": 3}
	for w, want := range cases {
		if got := estTokens(w); got != want {
			t.Errorf("estTokens(%q) = %d, want %d", w, got, want)
		}
	}
}
