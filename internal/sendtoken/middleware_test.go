package sendtoken

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
)

func authedReq(userID, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	if userID != "" {
		r = r.WithContext(auth.WithIdentity(r.Context(), auth.Identity{User: auth.User{ID: userID}}))
	}
	return r
}

func TestRequire(t *testing.T) {
	s := New("secret-key")
	tok := s.Issue("u1")

	// next records whether it ran and that it can still read the full body
	// (the middleware must restore it after peeking the token).
	var ran bool
	var seenBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.WriteHeader(http.StatusOK)
	})
	mw := s.Require()

	t.Run("valid token passes and body is restored", func(t *testing.T) {
		ran, seenBody = false, ""
		body := `{"send_token":"` + tok + `","body":"hello"}`
		mw(next).ServeHTTP(httptest.NewRecorder(), authedReq("u1", body))
		if !ran {
			t.Fatal("valid token should pass through")
		}
		if seenBody != body {
			t.Fatalf("body not restored for handler: got %q", seenBody)
		}
	})

	t.Run("invalid token blocked", func(t *testing.T) {
		ran = false
		mw(next).ServeHTTP(httptest.NewRecorder(), authedReq("u1", `{"send_token":"bad"}`))
		if ran {
			t.Fatal("invalid token must be blocked")
		}
	})

	t.Run("missing token blocked", func(t *testing.T) {
		ran = false
		mw(next).ServeHTTP(httptest.NewRecorder(), authedReq("u1", `{"body":"hi"}`))
		if ran {
			t.Fatal("missing token must be blocked")
		}
	})

	t.Run("another user's token blocked", func(t *testing.T) {
		ran = false
		body := `{"send_token":"` + s.Issue("u2") + `"}`
		mw(next).ServeHTTP(httptest.NewRecorder(), authedReq("u1", body))
		if ran {
			t.Fatal("token minted for a different user must be blocked")
		}
	})

	t.Run("unauthenticated passes through", func(t *testing.T) {
		ran = false
		mw(next).ServeHTTP(httptest.NewRecorder(), authedReq("", `{}`))
		if !ran {
			t.Fatal("unauth request should pass (auth handled elsewhere)")
		}
	})

	t.Run("nil signer passes through", func(t *testing.T) {
		ran = false
		var nilS *Signer
		nilS.Require()(next).ServeHTTP(httptest.NewRecorder(), authedReq("u1", `{}`))
		if !ran {
			t.Fatal("nil signer should disable the check")
		}
	})
}
