package sendtoken

import (
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestSigner_CurrentWindowValid(t *testing.T) {
	s := New("secret-key")
	tok := s.Issue("u1")
	if tok == "" {
		t.Fatal("empty token")
	}
	if !s.Valid("u1", tok) {
		t.Fatal("current-window token should be valid")
	}
}

func TestSigner_Rejects(t *testing.T) {
	s := New("secret-key")
	tok := s.Issue("u1")
	cases := []struct {
		name, user, token string
	}{
		{"different user", "u2", tok},
		{"empty token", "u1", ""},
		{"empty user", "", tok},
		{"garbage", "u1", "not-a-real-token"},
	}
	for _, c := range cases {
		if s.Valid(c.user, c.token) {
			t.Errorf("%s: expected invalid", c.name)
		}
	}
}

func TestSigner_DifferentKeysDiverge(t *testing.T) {
	a := New("key-a")
	b := New("key-b")
	if b.Valid("u1", a.Issue("u1")) {
		t.Fatal("token from a must not validate under b")
	}
}

func TestSigner_WindowAcceptance(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000*WindowSecs, 0)}
	s := New("secret-key")
	s.now = clk.now

	tok := s.Issue("u1") // window 0
	if !s.Valid("u1", tok) {
		t.Fatal("same window must be valid")
	}
	// Advance one window at a time; AcceptWindows=3 means current + 2 prior.
	clk.add(WindowSecs * time.Second) // 1 window old
	if !s.Valid("u1", tok) {
		t.Fatal("1-window-old token must still be valid")
	}
	clk.add(WindowSecs * time.Second) // 2 windows old
	if !s.Valid("u1", tok) {
		t.Fatal("2-window-old token must still be valid")
	}
	clk.add(WindowSecs * time.Second) // 3 windows old → out of acceptance
	if s.Valid("u1", tok) {
		t.Fatal("3-window-old token must be rejected")
	}
}
