package natsx

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

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
	return fmt.Sprintf("community.%s.chat", communityID)
}

// ChatNewSubject is the "a brand-new message just landed" fan-out — a
// strict subset of ChatSubject. Subscribers that want to ping/toast on
// real new messages (not edits, deletes, read-receipt updates) listen
// here. PostSend / Welcome / forum-bridge publish on this AND
// ChatSubject so chat-page re-renders stay correct.
func ChatNewSubject(communityID string) string {
	return fmt.Sprintf("community.%s.chat.new", communityID)
}

func ForumSubject(communityID string) string {
	return fmt.Sprintf("community.%s.forum", communityID)
}

func ForumThreadSubject(communityID, threadID string) string {
	return fmt.Sprintf("community.%s.forum.thread.%s", communityID, threadID)
}

func PresenceSubject(communityID string) string {
	return fmt.Sprintf("community.%s.presence", communityID)
}

// LobbySubject is the per-lobby fan-out subject for the guest-access
// feature. Per-lobby scoping (rather than per-community) keeps streams
// from waking on unrelated lobbies' messages.
func LobbySubject(communityID, lobbyID string) string {
	return fmt.Sprintf("community.%s.lobby.%s", communityID, lobbyID)
}

// MailboxSubject is the per-community fan-out for the global /inbox
// surface. The poll worker + filter CRUD endpoints publish here when a
// row changes; every viewer admin in this community wakes and
// re-renders.
func MailboxSubject(communityID string) string {
	return fmt.Sprintf("community.%s.mailbox", communityID)
}

// AgentThreadSubject is the per-thread fan-out for the Agent feature.
// The generation runner publishes the thread id here on every 100ms
// buffer flush; only the SSE streams open on that thread wake and
// re-render. Per-thread scoping keeps unrelated threads quiet.
func AgentThreadSubject(communityID, threadID string) string {
	return fmt.Sprintf("community.%s.agent.thread.%s", communityID, threadID)
}
