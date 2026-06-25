package connectors

import (
	"regexp"
	"strings"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/chat"
)

// EventAttachment is one file on a streamed message — a session-less,
// shared-signed URL plus metadata the worker can fetch directly. Filled by a
// ResolveAttachments closure (no uploads import here).
type EventAttachment struct {
	URL  string `json:"url"`
	MIME string `json:"mime"`
	Name string `json:"name"`
}

// StreamEvent is the JSON payload of one `event: message` SSE frame. It is the
// stable wire contract an external worker codes against — deliberately NOT our
// templ DOM (spec "Why JSON, not datastar"). Keep field names stable.
type StreamEvent struct {
	ID          string            `json:"id"`
	Channel     string            `json:"channel"` // channel slug
	ChannelID   string            `json:"channel_id"`
	Nick        string            `json:"nick"`      // author display name
	AuthorID    string            `json:"author_id"` // stable user id (for moderation actions); "" for author-less rows
	Kind        string            `json:"kind"`      // user | webhook | bot
	BodyMD      string            `json:"body_md"`
	BodyHTML    string            `json:"body_html"`
	Mentioned   bool              `json:"mentioned"` // body @mentions THIS connector
	ReplyTo     string            `json:"reply_to,omitempty"`
	CreatedAt   string            `json:"created_at"` // RFC3339 UTC
	Attachments []EventAttachment `json:"attachments,omitempty"`
}

// toEvent maps a chat message to its wire form for a given channel + mention
// state. Attachments are passed in already resolved (the stream resolves upload
// ids via a closure so this stays import-light and pure).
func toEvent(m chat.Message, channelSlug, channelName string, mentioned bool, atts []EventAttachment) StreamEvent {
	e := StreamEvent{
		ID:          m.ID,
		Channel:     channelSlug,
		ChannelID:   m.ChannelID,
		Nick:        m.AuthorName,
		Kind:        string(m.Kind),
		BodyMD:      m.BodyMarkdown,
		BodyHTML:    m.BodyHTML,
		Mentioned:   mentioned,
		CreatedAt:   m.CreatedAt.UTC().Format(time.RFC3339),
		Attachments: atts,
	}
	if m.AuthorID != nil {
		e.AuthorID = *m.AuthorID
	}
	if m.ReplyToID != nil {
		e.ReplyTo = *m.ReplyToID
	}
	return e
}

// mentionBoundary matches the character right after a candidate @name so the
// detector doesn't fire on a longer name ("@Acme" must not match inside
// "@AcmeBot"): end-of-string or a non-word, non-dash character ends a mention.
var mentionBoundary = regexp.MustCompile(`^[^\w-]`)

// Mentions reports whether body @mentions nick. Best-effort by design (spec
// Friction): a case-insensitive search for "@"+nick, requiring a word boundary
// after the name so "@Acme" doesn't match "@AcmeBot", but tolerant of nicks with
// spaces ("@Acme Support"). Not a parsed mention token — good enough to drive
// the mentions_only filter and the per-message `mentioned` flag.
func Mentions(body, nick string) bool {
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return false
	}
	hay := strings.ToLower(body)
	needle := "@" + strings.ToLower(nick)
	from := 0
	for {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return false
		}
		end := from + i + len(needle)
		// A match is valid when the name is followed by end-of-string or a
		// boundary char — so "@acme" in "@acmebot" (followed by 'b') is rejected.
		if end >= len(hay) || mentionBoundary.MatchString(hay[end:]) {
			return true
		}
		from = end
	}
}
