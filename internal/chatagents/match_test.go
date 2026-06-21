package chatagents

import (
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/agent"
)

func ag(name, mode, prefix string) agent.Agent {
	return agent.Agent{Name: name, TriggerMode: mode, TriggerPrefix: prefix}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		desc        string
		a           agent.Agent
		body        string
		multiPrefix bool
		want        bool
	}{
		// mention
		{"mention hit", ag("nick", agent.TriggerModeMention, "."), "hey @nick what's up", false, true},
		{"mention case-insensitive", ag("Nick", agent.TriggerModeMention, "."), "@nIcK hi", false, true},
		{"mention miss plain name", ag("nick", agent.TriggerModeMention, "."), "nick come here", false, false},
		{"mention miss different token", ag("nick", agent.TriggerModeMention, "."), "@nicky hi", false, false},
		{"mention does not fire on prefix", ag("nick", agent.TriggerModeMention, "."), ".nick hi", false, false},

		// prefix, lone agent (multiPrefix=false): any prefixed line summons it
		{"prefix lone bare", ag("nick", agent.TriggerModePrefix, "."), ".do the thing", false, true},
		{"prefix lone named", ag("nick", agent.TriggerModePrefix, "."), ".nick do it", false, true},
		{"prefix not at line start", ag("nick", agent.TriggerModePrefix, "."), "hey .nick", false, false},
		{"prefix miss no prefix", ag("nick", agent.TriggerModePrefix, "."), "@nick hi", false, false},
		{"prefix custom char", ag("nick", agent.TriggerModePrefix, "!"), "!summarize", false, true},
		{"prefix on second line", ag("nick", agent.TriggerModePrefix, "."), "context line\n.nick go", false, true},

		// prefix, multiple prefix-agents (multiPrefix=true): need <prefix><name>
		{"prefix multi explicit", ag("nick", agent.TriggerModePrefix, "."), ".nick do it", true, true},
		{"prefix multi bare ambiguous", ag("nick", agent.TriggerModePrefix, "."), ".do it", true, false},
		{"prefix multi other name", ag("nick", agent.TriggerModePrefix, "."), ".weather vilnius", true, false},
		{"prefix multi exact name only", ag("nick", agent.TriggerModePrefix, "."), ".nick", true, true},

		// both
		{"both via mention", ag("nick", agent.TriggerModeBoth, "."), "@nick hi", false, true},
		{"both via prefix", ag("nick", agent.TriggerModeBoth, "."), ".do x", false, true},
		{"both miss", ag("nick", agent.TriggerModeBoth, "."), "just chatting", false, false},

		// all
		{"all non-empty", ag("nick", agent.TriggerModeAll, "."), "anything", false, true},
		{"all empty", ag("nick", agent.TriggerModeAll, "."), "   ", false, false},

		// default prefix fallback
		{"prefix empty defaults to dot", ag("nick", agent.TriggerModePrefix, ""), ".hello", false, true},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			if got := Match(c.a, c.body, c.multiPrefix); got != c.want {
				t.Fatalf("Match(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestCountPrefixAgents(t *testing.T) {
	agents := []agent.Agent{
		ag("a", agent.TriggerModeMention, "."),
		ag("b", agent.TriggerModePrefix, "."),
		ag("c", agent.TriggerModeBoth, "."),
		ag("d", agent.TriggerModeAll, "."),
	}
	if n := countPrefixAgents(agents); n != 2 {
		t.Fatalf("countPrefixAgents = %d, want 2 (prefix + both)", n)
	}
}
