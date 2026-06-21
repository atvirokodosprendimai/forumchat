// Package chatagents makes a community's ai_agents participate in the live chat
// channel: it matches a member's message against each bound agent's trigger
// config and drives a streaming kind='bot' reply. It is the seam between
// internal/chat (the bubble + fan-out) and internal/agent (the LLM provider) —
// importing both, so neither has to import the other.
package chatagents

import (
	"strings"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
)

// Match reports whether body should trigger agent a, per a.TriggerMode.
// multiPrefix is true when more than one prefix-triggered agent shares the
// channel: a bare "<prefix> …" is then ambiguous, so a prefix match requires
// the explicit "<prefix><name>" form.
func Match(a agent.Agent, body string, multiPrefix bool) bool {
	switch a.TriggerMode {
	case agent.TriggerModeAll:
		return strings.TrimSpace(body) != ""
	case agent.TriggerModePrefix:
		return matchPrefix(a, body, multiPrefix)
	case agent.TriggerModeBoth:
		return matchMention(a, body) || matchPrefix(a, body, multiPrefix)
	default: // TriggerModeMention (and any unknown value) → mention only
		return matchMention(a, body)
	}
}

// matchMention is true when body @mentions the agent by name.
func matchMention(a agent.Agent, body string) bool {
	name := strings.ToLower(strings.TrimSpace(a.Name))
	if name == "" {
		return false
	}
	for _, tok := range mentionTokens(body) {
		if tok == name {
			return true
		}
	}
	return false
}

// matchPrefix is true when a line of body starts with the agent's trigger
// prefix. With multiPrefix the line must read "<prefix><name> …" to pick this
// agent; otherwise a bare "<prefix> …" summons the lone prefix-agent.
func matchPrefix(a agent.Agent, body string, multiPrefix bool) bool {
	prefix := a.TriggerPrefix
	if prefix == "" {
		prefix = "."
	}
	name := strings.ToLower(strings.TrimSpace(a.Name))
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if !multiPrefix {
			return true
		}
		rest := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		if rest == name || strings.HasPrefix(rest, name+" ") {
			return true
		}
	}
	return false
}

// mentionTokens extracts @name tokens from body. Mirrors chat.parseMentions:
// a contiguous run of [a-zA-Z0-9_-] of length >= 2 after '@', lowercased.
func mentionTokens(body string) []string {
	if body == "" {
		return nil
	}
	var out []string
	var b strings.Builder
	in := false
	flush := func() {
		if b.Len() >= 2 {
			out = append(out, strings.ToLower(b.String()))
		}
		b.Reset()
	}
	for _, r := range body {
		if r == '@' {
			in = true
			b.Reset()
			continue
		}
		if in {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				b.WriteRune(r)
				continue
			}
			flush()
			in = false
		}
	}
	if in {
		flush()
	}
	return out
}

// countPrefixAgents counts the agents in a set whose trigger involves a prefix
// (prefix or both). >1 means prefix matches need explicit "<prefix><name>".
func countPrefixAgents(agents []agent.Agent) int {
	n := 0
	for _, a := range agents {
		if a.TriggerMode == agent.TriggerModePrefix || a.TriggerMode == agent.TriggerModeBoth {
			n++
		}
	}
	return n
}
