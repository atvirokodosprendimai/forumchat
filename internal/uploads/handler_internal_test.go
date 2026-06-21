package uploads

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

func newSigStore() *Store {
	return NewStore(nil, "/tmp", 1<<20, "test-sign-key")
}

func TestHasValidSharedSig(t *testing.T) {
	s := newSigStore()
	const id = "upload-1"

	mint := func(exp time.Time) string { return s.SignShared(id, exp) }

	t.Run("valid shared sig admits", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		r := httptest.NewRequest("GET", fmt.Sprintf("/uploads/%s?exp=%d&sig=%s", id, exp.Unix(), mint(exp)), nil)
		if !hasValidSharedSig(s, r, id) {
			t.Fatal("valid shared sig should admit")
		}
	})

	t.Run("missing sig rejected", func(t *testing.T) {
		r := httptest.NewRequest("GET", "/uploads/"+id, nil)
		if hasValidSharedSig(s, r, id) {
			t.Fatal("no sig must be rejected")
		}
	})

	t.Run("expired sig rejected", func(t *testing.T) {
		exp := time.Now().Add(-time.Hour)
		r := httptest.NewRequest("GET", fmt.Sprintf("/uploads/%s?exp=%d&sig=%s", id, exp.Unix(), mint(exp)), nil)
		if hasValidSharedSig(s, r, id) {
			t.Fatal("expired sig must be rejected")
		}
	})

	t.Run("tampered sig rejected", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		r := httptest.NewRequest("GET", fmt.Sprintf("/uploads/%s?exp=%d&sig=deadbeef", id, exp.Unix()), nil)
		if hasValidSharedSig(s, r, id) {
			t.Fatal("tampered sig must be rejected")
		}
	})

	t.Run("sig for different upload rejected", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		other := s.SignShared("other-upload", exp)
		r := httptest.NewRequest("GET", fmt.Sprintf("/uploads/%s?exp=%d&sig=%s", id, exp.Unix(), other), nil)
		if hasValidSharedSig(s, r, id) {
			t.Fatal("sig bound to a different upload must be rejected")
		}
	})
}
