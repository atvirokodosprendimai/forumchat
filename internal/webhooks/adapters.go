package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Rendered is an adapter's output. Skip=true means "valid request, but nothing
// worth posting" (health pings, empty bodies) — the handler returns 200 and
// posts no message.
//
// ThreadKey, when non-empty, routes the message to the forum instead of the
// chat channel: it is the external thread-root identifier (e.g. a Matrix
// thread-root event id) that forumchat maps to a forum thread. Subject seeds a
// newly opened thread; Author overrides the bot display name for this one
// message (the human who spoke on the far side).
//
// MessageKey / ReplyToKey drive INLINE chat threading (the alternative to the
// ThreadKey forum path). MessageKey is this message's own external id; forumchat
// records it so a later message can target it. ReplyToKey is the external id of
// an earlier message this one replies to — forumchat resolves it to the prior
// chat message and posts this one as an inline reply under it. A message with no
// ReplyToKey stays flat. Both are no-ops on the forum (ThreadKey) path.
type Rendered struct {
	Markdown   string
	Skip       bool
	ThreadKey  string
	Subject    string
	Author     string
	MessageKey string
	ReplyToKey string
}

// Adapter turns an inbound webhook request into a chat-ready markdown body.
// Implementations are pure functions of (header, body) — no DB, no I/O — so
// they unit-test against captured fixtures.
type Adapter interface {
	Parse(h http.Header, body []byte) (Rendered, error)
}

// adapterFor returns the adapter for a provider. Unknown providers fall back to
// generic, so a new provider degrades gracefully instead of 500ing.
func adapterFor(provider string) Adapter {
	switch provider {
	case "github":
		return githubAdapter{}
	default:
		return genericAdapter{}
	}
}

// genericAdapter accepts Slack/Discord-style {"text":...} or {"content":...}
// JSON, and otherwise fences the raw body as a code block. This is the Matrix
// path (hookshot/maubot fire a generic JSON POST) and the catch-all for any
// system that can be pointed at a URL.
type genericAdapter struct{}

func (genericAdapter) Parse(_ http.Header, body []byte) (Rendered, error) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return Rendered{Skip: true}, nil
	}
	var p struct {
		Text       string `json:"text"`
		Content    string `json:"content"`
		ThreadKey  string `json:"thread_key"`
		Subject    string `json:"subject"`
		Author     string `json:"author"`
		MessageKey string `json:"message_key"`
		ReplyToKey string `json:"reply_to_key"`
	}
	if err := json.Unmarshal(body, &p); err == nil {
		meta := Rendered{
			ThreadKey:  strings.TrimSpace(p.ThreadKey),
			Subject:    strings.TrimSpace(p.Subject),
			Author:     strings.TrimSpace(p.Author),
			MessageKey: strings.TrimSpace(p.MessageKey),
			ReplyToKey: strings.TrimSpace(p.ReplyToKey),
		}
		if t := strings.TrimSpace(p.Text); t != "" {
			meta.Markdown = t
			return meta, nil
		}
		if c := strings.TrimSpace(p.Content); c != "" {
			meta.Markdown = c
			return meta, nil
		}
	}
	// Not the expected JSON shape (or no text/content): post the raw payload
	// fenced so it renders verbatim instead of being interpreted as markdown.
	return Rendered{Markdown: "```\n" + trimmed + "\n```"}, nil
}

// githubAdapter formats common GitHub webhook events. It switches on the
// X-GitHub-Event header; unknown events get a one-line fallback and ping is
// skipped.
type githubAdapter struct{}

func (githubAdapter) Parse(h http.Header, body []byte) (Rendered, error) {
	event := h.Get("X-GitHub-Event")
	if event == "ping" {
		return Rendered{Skip: true}, nil
	}
	var p githubPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Rendered{}, fmt.Errorf("github: decode: %w", err)
	}
	repo := p.Repository.FullName
	switch event {
	case "push":
		n := len(p.Commits)
		ref := strings.TrimPrefix(p.Ref, "refs/heads/")
		head := "**" + actor(p) + "** pushed " + plural(n, "commit") + " to `" + repo + "`@`" + ref + "`"
		var b strings.Builder
		b.WriteString(head)
		for i, c := range p.Commits {
			if i == 5 {
				b.WriteString(fmt.Sprintf("\n- …and %d more", n-5))
				break
			}
			b.WriteString("\n- " + firstLine(c.Message))
			if c.ID != "" {
				b.WriteString(" (`" + shortSHA(c.ID) + "`)")
			}
		}
		return Rendered{Markdown: b.String()}, nil
	case "pull_request":
		title := p.PullRequest.Title
		link := mdLink(fmt.Sprintf("#%d", p.PullRequest.Number), p.PullRequest.HTMLURL)
		verb := p.Action
		if p.Action == "closed" && p.PullRequest.Merged {
			verb = "merged"
		}
		return Rendered{Markdown: fmt.Sprintf("**%s** %s PR %s in `%s` — %s",
			actor(p), verb, link, repo, title)}, nil
	case "issues":
		link := mdLink(fmt.Sprintf("#%d", p.Issue.Number), p.Issue.HTMLURL)
		return Rendered{Markdown: fmt.Sprintf("**%s** %s issue %s in `%s` — %s",
			actor(p), p.Action, link, repo, p.Issue.Title)}, nil
	case "release":
		name := p.Release.Name
		if name == "" {
			name = p.Release.TagName
		}
		return Rendered{Markdown: fmt.Sprintf("**%s** %s release %s in `%s`",
			actor(p), p.Action, mdLink(name, p.Release.HTMLURL), repo)}, nil
	default:
		if repo != "" {
			return Rendered{Markdown: fmt.Sprintf("GitHub `%s` event on `%s`", event, repo)}, nil
		}
		return Rendered{Markdown: "GitHub `" + event + "` event"}, nil
	}
}

type githubPayload struct {
	Action     string `json:"action"`
	Ref        string `json:"ref"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
	Pusher struct {
		Name string `json:"name"`
	} `json:"pusher"`
	Commits []struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"commits"`
	PullRequest struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
		Merged  bool   `json:"merged"`
	} `json:"pull_request"`
	Issue struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	} `json:"issue"`
	Release struct {
		Name    string `json:"name"`
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	} `json:"release"`
}

func actor(p githubPayload) string {
	if p.Sender.Login != "" {
		return p.Sender.Login
	}
	if p.Pusher.Name != "" {
		return p.Pusher.Name
	}
	return "someone"
}

// verifyGitHubSignature compares the X-Hub-Signature-256 header against an
// HMAC-SHA256 of body keyed by secret, in constant time.
func verifyGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// verifyGenericSignature compares the X-Signature header against an HMAC-SHA256
// of body keyed by secret, in constant time (FIX1 H4). The header may be a bare
// hex digest or "sha256=<hex>", matching what most senders emit.
func verifyGenericSignature(secret string, body []byte, header string) bool {
	header = strings.TrimSpace(strings.TrimPrefix(header, "sha256="))
	want, err := hex.DecodeString(header)
	if err != nil || len(want) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// verifyInboundSignature gates a signed inbound webhook: github uses its
// X-Hub-Signature-256 header; every other provider uses a generic X-Signature
// HMAC. Only called when a secret is configured (FIX1 H4) — an unsigned webhook
// is authenticated by its URL token alone, as before.
func verifyInboundSignature(wh Webhook, body []byte, hdr http.Header) bool {
	if wh.Provider == "github" {
		return verifyGitHubSignature(wh.Secret, body, hdr.Get("X-Hub-Signature-256"))
	}
	return verifyGenericSignature(wh.Secret, body, hdr.Get("X-Signature"))
}

func mdLink(text, href string) string {
	if href == "" {
		return text
	}
	return "[" + text + "](" + href + ")"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func shortSHA(id string) string {
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
