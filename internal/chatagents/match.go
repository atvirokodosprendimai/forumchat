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

// matchMention is true when body @mentions the agent by its FULL name —
// spaces included. Because the matcher already knows the exact name, it looks
// for "@<name>" directly rather than tokenising on word boundaries (a bot named
// "nick name here" must match "@nick name here", not just "@nick"). A trailing
// word char fails the match so "@nick" never matches an agent named "nicky".
func matchMention(a agent.Agent, body string) bool {
	name := strings.ToLower(strings.TrimSpace(a.Name))
	if name == "" {
		return false
	}
	hay := strings.ToLower(body)
	needle := "@" + name
	for from := 0; ; {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(needle)
		if end >= len(hay) || !isMentionWordChar(hay[end]) {
			return true
		}
		from = i + 1
	}
}

// isMentionWordChar reports whether b continues a mention name token — used to
// reject a partial match ("@nick" inside "@nickname").
func isMentionWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
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
