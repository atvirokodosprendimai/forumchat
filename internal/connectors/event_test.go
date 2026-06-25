package connectors

import "testing"

func TestMentions(t *testing.T) {
	cases := []struct {
		name, body, nick string
		want             bool
	}{
		{"plain", "hey @Acme can you help", "Acme", true},
		{"case-insensitive", "hey @acme", "Acme", true},
		{"end of string", "ping @Acme", "Acme", true},
		{"punctuation boundary", "@Acme, hello", "Acme", true},
		{"longer name not matched", "talk to @AcmeBot please", "Acme", false},
		{"no at-sign", "Acme is great", "Acme", false},
		{"absent", "nothing here", "Acme", false},
		{"nick with space", "thanks @Acme Support!", "Acme Support", true},
		{"empty nick", "@", "", false},
		{"second occurrence after a near-miss", "@AcmeBot and @Acme", "Acme", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Mentions(c.body, c.nick); got != c.want {
				t.Fatalf("Mentions(%q, %q) = %v, want %v", c.body, c.nick, got, c.want)
			}
		})
	}
}

func TestNormalizeCapabilities(t *testing.T) {
	// De-dups, drops unknown/garbage tokens, lower-cases, and sorts.
	got := normalizeCapabilities([]string{"send", "SEND", "delete", "fly", "", "  ban  "})
	want := []string{"ban", "delete", "send"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// CSV round-trip.
	if c := (Connector{Capabilities: splitCapabilities(joinCapabilities(want))}); !c.Can(CapBan) || c.Can("fly") {
		t.Fatalf("capability round-trip lost data: %v", c.Capabilities)
	}
}
