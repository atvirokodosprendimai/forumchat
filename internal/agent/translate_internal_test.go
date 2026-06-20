package agent

import "testing"

func TestParseTranslations(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "plain lines",
			in:   "Hello there\nHi there\nGood day",
			want: []string{"Hello there", "Hi there", "Good day"},
		},
		{
			name: "strips numbering and bullets",
			in:   "1. Hello\n2) Hi\n- Hey\n* Yo",
			want: []string{"Hello", "Hi", "Hey"}, // capped at 3
		},
		{
			name: "strips wrapping quotes",
			in:   "\"Hello\"\n'Hi'\n`Hey`",
			want: []string{"Hello", "Hi", "Hey"},
		},
		{
			name: "drops blanks and dedupes case-insensitively",
			in:   "Hello\n\n  \nhello\nHELLO\nHi",
			want: []string{"Hello", "Hi"},
		},
		{
			name: "caps at three",
			in:   "a\nb\nc\nd\ne",
			want: []string{"a", "b", "c"},
		},
		{
			name: "empty input",
			in:   "",
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTranslations(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("parseTranslations(%q) = %v (len %d), want %v (len %d)",
					tt.in, got, len(got), tt.want, len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTranslateEmptyShortCircuits(t *testing.T) {
	// Empty text or empty model must return (nil, nil) without dialing Ollama.
	if got, err := Translate(t.Context(), "http://127.0.0.1:1", "model", "   "); err != nil || got != nil {
		t.Fatalf("Translate(empty text) = (%v, %v), want (nil, nil)", got, err)
	}
	if got, err := Translate(t.Context(), "http://127.0.0.1:1", "  ", "hello"); err != nil || got != nil {
		t.Fatalf("Translate(empty model) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestStripListMarker(t *testing.T) {
	cases := map[string]string{
		"1. x":   "x",
		"22) y":  "y",
		"- z":    "z",
		"* w":    "w",
		"plain":  "plain",
		"1.x":    "1.x", // no space → not a marker
		"1.2 km": "1.2 km",
	}
	for in, want := range cases {
		if got := stripListMarker(in); got != want {
			t.Errorf("stripListMarker(%q) = %q, want %q", in, got, want)
		}
	}
	// sanity: cleanTranslationLine composes strip + quote-trim
	if got := cleanTranslationLine(`  2) "Done"  `); got != "Done" {
		t.Errorf("cleanTranslationLine = %q, want %q", got, "Done")
	}
}
