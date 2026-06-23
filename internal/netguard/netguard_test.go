package netguard

import "testing"

func TestBlockedURL(t *testing.T) {
	cases := []struct {
		url     string
		blocked bool
	}{
		{"", false},                          // unset is allowed
		{"http://169.254.169.254/latest", true}, // cloud metadata (link-local)
		{"http://127.0.0.1:11434", true},     // loopback
		{"https://10.1.2.3:6333", true},      // private
		{"http://192.168.0.5", true},         // private
		{"http://[::1]:6333", true},          // ipv6 loopback
		{"http://8.8.8.8:11434", false},      // public ipv4 literal
		{"ftp://8.8.8.8", true},              // wrong scheme
		{"://nonsense", true},                // malformed
	}
	for _, c := range cases {
		got, reason := BlockedURL(c.url)
		if got != c.blocked {
			t.Errorf("BlockedURL(%q) = %v (%s), want %v", c.url, got, reason, c.blocked)
		}
	}
}
