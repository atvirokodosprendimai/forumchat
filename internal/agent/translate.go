package agent

import (
	"context"
	"strings"
	"time"
)

// translateSystemPrompt steers the model to behave as a pure translation
// engine: detect the source language, translate to English, and emit up to
// three faithful alternatives one per line with no decoration. The parser
// (parseTranslations) tolerates stray numbering/quotes anyway, but a clean
// prompt keeps small local models on the rails.
const translateSystemPrompt = `You are a translation engine. Detect the source language of the user's message and translate it into English.
Reply with up to 3 faithful alternative English translations, the most accurate first.
Put each translation on its own line. No numbering, no bullet points, no quotation marks, no commentary, no blank lines.
If the message is already English, reply with up to 3 natural English rephrasings.`

// translateTimeout bounds a single translation request so a slow or
// unreachable Ollama daemon can't hang the composer popup open indefinitely.
const translateTimeout = 25 * time.Second

// Translate auto-detects the source language of text and returns up to three
// faithful English translations, best first. It runs one non-streamed,
// tool-less Ollama turn against baseURL/model and parses the newline-delimited
// reply. Empty text returns (nil, nil); a provider error is propagated. The
// returned slice is never longer than 3 and contains no blanks or duplicates.
func Translate(ctx context.Context, baseURL, model, text string) ([]string, error) {
	text = strings.TrimSpace(text)
	if text == "" || strings.TrimSpace(model) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, translateTimeout)
	defer cancel()

	o := NewOllama(baseURL)
	msgs := []ChatMessage{
		{Role: "system", Content: translateSystemPrompt},
		{Role: "user", Content: text},
	}
	var b strings.Builder
	if _, err := o.Stream(ctx, model, msgs, nil, func(d string) error {
		b.WriteString(d)
		return nil
	}); err != nil {
		return nil, err
	}
	return parseTranslations(b.String()), nil
}

// parseTranslations splits the model reply into clean candidate lines: it
// strips leading list markers ("1.", "2)", "-", "*") and surrounding quotes,
// drops blanks, de-duplicates case-insensitively, and caps the result at 3.
func parseTranslations(s string) []string {
	out := make([]string, 0, 3)
	seen := make(map[string]struct{}, 3)
	for _, raw := range strings.Split(s, "\n") {
		line := cleanTranslationLine(raw)
		if line == "" {
			continue
		}
		key := strings.ToLower(line)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, line)
		if len(out) == 3 {
			break
		}
	}
	return out
}

// cleanTranslationLine removes one leading list marker and wrapping quotes
// from a single reply line, returning the bare translation.
func cleanTranslationLine(line string) string {
	line = strings.TrimSpace(line)
	line = stripListMarker(line)
	line = strings.TrimSpace(line)
	// Strip a single pair of wrapping quotes if present.
	if len(line) >= 2 {
		f, l := line[0], line[len(line)-1]
		if (f == '"' && l == '"') || (f == '\'' && l == '\'') || (f == '`' && l == '`') {
			line = strings.TrimSpace(line[1 : len(line)-1])
		}
	}
	return line
}

// stripListMarker drops a leading "N." / "N)" / "-" / "*" / "•" ordered or
// bulleted-list prefix (with its trailing space), if any.
func stripListMarker(line string) string {
	switch {
	case strings.HasPrefix(line, "- "), strings.HasPrefix(line, "* "), strings.HasPrefix(line, "• "):
		return line[2:]
	}
	// Numbered: leading digits then '.' or ')' then space.
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i < len(line) && (line[i] == '.' || line[i] == ')') {
		rest := line[i+1:]
		if strings.HasPrefix(rest, " ") {
			return rest[1:]
		}
	}
	return line
}
