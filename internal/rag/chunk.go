package rag

import (
	"strings"
	"unicode/utf8"
)

// ChunkConfig bounds the bge-m3 sliding window. bge-m3 accepts ~8k tokens but we
// treat 4k as the working budget: BodyTokens of primary content per chunk, plus
// Overlap tokens of context bled in on each side (so a chunk embeds with the
// surrounding text but the window advances by BodyTokens), leaving headroom for
// the model's own special tokens. With the defaults (2800 + 400/400) a chunk is
// ~3600 tokens, ~400 under 4k.
type ChunkConfig struct {
	BodyTokens int
	Overlap    int
}

// chunk splits a doc into overlapping windows. Title is prepended once (it is
// cheap context that helps short bodies embed meaningfully). Content shorter than
// BodyTokens returns a single chunk with no overlap math. Returns nil for empty
// input.
//
// Token counts are estimated (estTokens) rather than tokenized — bge-m3 uses an
// XLM-RoBERTa SentencePiece vocab we don't ship; the estimate is deliberately
// conservative (rounds up) so we stay under budget. Swap estTokens for a real
// tokenizer later without touching callers.
func chunk(title, body string, cfg ChunkConfig) []string {
	if cfg.BodyTokens <= 0 {
		cfg.BodyTokens = 2800
	}
	if cfg.Overlap < 0 {
		cfg.Overlap = 0
	}
	text := strings.TrimSpace(body)
	if t := strings.TrimSpace(title); t != "" {
		text = strings.TrimSpace(t + "\n\n" + text)
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}

	tok := make([]int, len(words))
	total := 0
	for i, w := range words {
		tok[i] = estTokens(w)
		total += tok[i]
	}
	if total <= cfg.BodyTokens {
		return []string{strings.Join(words, " ")}
	}

	var chunks []string
	i := 0
	for i < len(words) {
		j := advance(tok, i, cfg.BodyTokens) // primary slice [i, j)
		if j == i {                          // a single oversized word — force progress
			j = i + 1
		}
		lo := back(tok, i, cfg.Overlap)    // bleed context before
		hi := advance(tok, j, cfg.Overlap) // bleed context after
		chunks = append(chunks, strings.Join(words[lo:hi], " "))
		i = j
	}
	return chunks
}

// estTokens approximates the SentencePiece token count of one whitespace word as
// ceil(runes/4), at least 1. Short words → 1 token; long words → proportionally
// more, mirroring subword splitting.
func estTokens(word string) int {
	n := utf8.RuneCountInString(word)
	if n <= 4 {
		return 1
	}
	return (n + 3) / 4
}

// advance returns the smallest index k >= from such that the token sum of
// words[from:k] is >= budget, or len(tok) if the tail fits.
func advance(tok []int, from, budget int) int {
	sum := 0
	for k := from; k < len(tok); k++ {
		sum += tok[k]
		if sum >= budget {
			return k + 1
		}
	}
	return len(tok)
}

// back returns the largest index k <= from such that the token sum of
// words[k:from] is <= budget (how far back we can bleed context).
func back(tok []int, from, budget int) int {
	if budget <= 0 {
		return from
	}
	sum := 0
	k := from
	for k > 0 {
		sum += tok[k-1]
		if sum > budget {
			break
		}
		k--
	}
	return k
}
