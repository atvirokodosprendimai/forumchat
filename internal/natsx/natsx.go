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
