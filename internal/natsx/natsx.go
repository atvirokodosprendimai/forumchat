package natsx

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// safeSubjectID neutralises any byte that carries meaning in a NATS subject —
// the `.` token separator and the `*`/`>` wildcards — so an id can never inject
// extra subject levels or a wildcard subscription (FIX1 M15). Ids are uuids /
// slugs in practice, so this is defense-in-depth and a no-op for well-formed
// ids. Disallowed bytes map to `_`, keeping the mapping deterministic so
// publisher and subscriber derive the same subject.
func safeSubjectID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == ':':
			return r
		default:
			return '_'
		}
	}, id)
}

func Connect(url string, log *slog.Logger) (*nats.Conn, error) {
	opts := []nats.Option{
		nats.Name("forumchat"),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			log.Warn("nats disconnected", "err", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("nats reconnected", "url", nc.ConnectedUrl())
		}),
		nats.ClosedHandler(func(_ *nats.Conn) {
			log.Warn("nats connection closed")
		}),
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats connect %s: %w", url, err)
	}
	return nc, nil
}

func ChatSubject(communityID string) string {
	return fmt.Sprintf("community.%s.chat", safeSubjectID(communityID))
}

// AllChatSubject is the wildcard that matches every community's ChatSubject
// (`community.*.chat`). The platform super-admin's cross-community chat inbox
// subscribes here to fan in chat writes from every community across processes.
func AllChatSubject() string { return "community.*.chat" }

// ChatNewSubject is the "a brand-new message just landed" fan-out — a
// strict subset of ChatSubject. Subscribers that want to ping/toast on
// real new messages (not edits, deletes, read-receipt updates) listen
// here. PostSend / Welcome / forum-bridge publish on this AND
// ChatSubject so chat-page re-renders stay correct.
func ChatNewSubject(communityID string) string {
	return fmt.Sprintf("community.%s.chat.new", safeSubjectID(communityID))
}

func ForumSubject(communityID string) string {
	return fmt.Sprintf("community.%s.forum", safeSubjectID(communityID))
}

func ForumThreadSubject(communityID, threadID string) string {
	return fmt.Sprintf("community.%s.forum.thread.%s", safeSubjectID(communityID), safeSubjectID(threadID))
}

func PresenceSubject(communityID string) string {
	return fmt.Sprintf("community.%s.presence", safeSubjectID(communityID))
}

// LobbySubject is the per-lobby fan-out subject for the guest-access
// feature. Per-lobby scoping (rather than per-community) keeps streams
// from waking on unrelated lobbies' messages.
func LobbySubject(communityID, lobbyID string) string {
	return fmt.Sprintf("community.%s.lobby.%s", safeSubjectID(communityID), safeSubjectID(lobbyID))
}

// NoteSubject is the per-note fan-out for collaborative editing. Each editor's
// collab SSE stream subscribes here; a merged edit or a Save publishes so every
// open editor (across processes) re-syncs.
func NoteSubject(communityID, noteID string) string {
	return fmt.Sprintf("community.%s.note.%s", safeSubjectID(communityID), safeSubjectID(noteID))
}

// MailboxSubject is the per-community fan-out for the global /inbox
// surface. The poll worker + filter CRUD endpoints publish here when a
// row changes; every viewer admin in this community wakes and
// re-renders.
func MailboxSubject(communityID string) string {
	return fmt.Sprintf("community.%s.mailbox", safeSubjectID(communityID))
}

// AgentThreadSubject is the per-thread fan-out for the Agent feature.
// The generation runner publishes the thread id here on every 100ms
// buffer flush; only the SSE streams open on that thread wake and
// re-render. Per-thread scoping keeps unrelated threads quiet.
func AgentThreadSubject(communityID, threadID string) string {
	return fmt.Sprintf("community.%s.agent.thread.%s", safeSubjectID(communityID), safeSubjectID(threadID))
}
