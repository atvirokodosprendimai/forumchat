package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

func TestGenericAdapter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		skip bool
	}{
		{"text field", `{"text":"hello world"}`, "hello world", false},
		{"content field", `{"content":"yo"}`, "yo", false},
		{"text beats content", `{"text":"a","content":"b"}`, "a", false},
		{"raw non-json fenced", `not json`, "```\nnot json\n```", false},
		{"empty skips", ``, "", true},
		{"blank skips", `   `, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := genericAdapter{}.Parse(http.Header{}, []byte(c.body))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Skip != c.skip {
				t.Fatalf("skip = %v, want %v", got.Skip, c.skip)
			}
			if !c.skip && got.Markdown != c.want {
				t.Fatalf("markdown = %q, want %q", got.Markdown, c.want)
			}
		})
	}
}

func TestGithubAdapterPing(t *testing.T) {
	h := http.Header{}
	h.Set("X-GitHub-Event", "ping")
	got, err := githubAdapter{}.Parse(h, []byte(`{"zen":"keep it logically awesome"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Skip {
		t.Fatalf("ping should skip, got %q", got.Markdown)
	}
}

func TestGithubAdapterPush(t *testing.T) {
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	body := `{
		"ref":"refs/heads/main",
		"repository":{"full_name":"acme/widgets"},
		"sender":{"login":"alice"},
		"commits":[
			{"id":"abcdef1234567890","message":"fix: thing\n\ndetails"},
			{"id":"1122334455667788","message":"docs: readme"}
		]
	}`
	got, err := githubAdapter{}.Parse(h, []byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Skip {
		t.Fatal("push should not skip")
	}
	for _, want := range []string{"alice", "2 commits", "acme/widgets", "main", "fix: thing", "abcdef1"} {
		if !strings.Contains(got.Markdown, want) {
			t.Fatalf("push markdown %q missing %q", got.Markdown, want)
		}
	}
}

func TestGithubAdapterPullRequestMerged(t *testing.T) {
	h := http.Header{}
	h.Set("X-GitHub-Event", "pull_request")
	body := `{
		"action":"closed",
		"repository":{"full_name":"acme/widgets"},
		"sender":{"login":"bob"},
		"pull_request":{"number":42,"title":"Add gizmo","html_url":"https://gh/pr/42","merged":true}
	}`
	got, err := githubAdapter{}.Parse(h, []byte(body))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"bob", "merged", "#42", "Add gizmo"} {
		if !strings.Contains(got.Markdown, want) {
			t.Fatalf("PR markdown %q missing %q", got.Markdown, want)
		}
	}
}

func TestGithubAdapterBadJSON(t *testing.T) {
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	if _, err := (githubAdapter{}).Parse(h, []byte(`{not json`)); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestVerifyGitHubSignature(t *testing.T) {
	secret := "s3cr3t"
	body := []byte(`{"hello":"world"}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyGitHubSignature(secret, body, good) {
		t.Fatal("valid signature rejected")
	}
	if verifyGitHubSignature(secret, body, "sha256=deadbeef") {
		t.Fatal("invalid signature accepted")
	}
	if verifyGitHubSignature(secret, body, "") {
		t.Fatal("empty signature accepted")
	}
	if verifyGitHubSignature("wrong", body, good) {
		t.Fatal("wrong-key signature accepted")
	}
}
