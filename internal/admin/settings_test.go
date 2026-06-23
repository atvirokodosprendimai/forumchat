package admin

import "testing"

// TestOverrideURL locks the fix for the SSRF guard firing on the platform's own
// default host: the owner Settings form pre-fills each URL field with the
// resolved effective value, so saving after changing only the join policy
// re-submits the platform default (e.g. a localhost Ollama/Qdrant host). That is
// not a tenant override and must normalize back to "" (inherit) so netguard
// never inspects it.
func TestOverrideURL(t *testing.T) {
	const def = "http://localhost:11434"
	cases := []struct {
		name      string
		submitted string
		def       string
		want      string
	}{
		{"blank inherits", "", def, ""},
		{"whitespace inherits", "   ", def, ""},
		{"equals default inherits", def, def, ""},
		{"equals default after trim inherits", "  " + def + " ", def, ""},
		{"genuine override kept", "https://ollama.example.com", def, "https://ollama.example.com"},
		{"internal override kept for guard to reject", "http://10.0.0.5:6333", def, "http://10.0.0.5:6333"},
		{"empty default, blank field", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := overrideURL(tc.submitted, tc.def); got != tc.want {
				t.Fatalf("overrideURL(%q, %q) = %q, want %q", tc.submitted, tc.def, got, tc.want)
			}
		})
	}
}
