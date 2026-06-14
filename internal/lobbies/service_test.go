package lobbies_test

import (
	"context"
	"testing"
	"time"

	"github.com/atvirokodosprendimai/forumchat/internal/lobbies"
)

// fakeInvites mocks the InviteIssuer interface so service_test can run
// without spinning up the full auth.Service.
type fakeInvites struct {
	lastCommunity string
	called        int
}

func (f *fakeInvites) IssueInvite(_ context.Context, communityID string, _ *string, _ *int) (string, error) {
	f.called++
	f.lastCommunity = communityID
	return "FAKE-CODE-XYZ", nil
}

func setupService(t *testing.T) (*lobbies.Service, string, string, *fakeInvites) {
	t.Helper()
	repo, cid, hostID := setupRepo(t)
	inv := &fakeInvites{}
	return lobbies.NewService(repo, inv), cid, hostID, inv
}

func TestMint_DefaultsAndToken(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, err := svc.Mint(context.Background(), lobbies.MintInput{
		CommunityID: cid, HostUserID: hostID,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if l.Medium != lobbies.MediumLobby {
		t.Errorf("default medium want lobby, got %q", l.Medium)
	}
	if l.Status != lobbies.StatusOpen {
		t.Errorf("status want open, got %q", l.Status)
	}
	if len(l.GuestToken) < 20 {
		t.Errorf("token too short: %q", l.GuestToken)
	}
	if l.ExpiresAt != nil {
		t.Errorf("expected no expiry, got %v", l.ExpiresAt)
	}
}

func TestMint_RejectsUnknownMedium(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	_, err := svc.Mint(context.Background(), lobbies.MintInput{
		CommunityID: cid, HostUserID: hostID, Medium: "bogus",
	})
	if err == nil {
		t.Fatal("want error for unknown medium")
	}
}

func TestSend_PersistsAndRendersMarkdown(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{CommunityID: cid, HostUserID: hostID})
	host := hostID
	m, err := svc.Send(context.Background(), lobbies.SendInput{
		LobbyID:      l.ID,
		AuthorKind:   lobbies.AuthorHost,
		AuthorUserID: &host,
		BodyMarkdown: "**hi** there",
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if m.BodyHTML == "" || m.BodyHTML == m.BodyMarkdown {
		t.Errorf("expected rendered HTML, got %q", m.BodyHTML)
	}
}

func TestSend_RejectsClosedLobby(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{CommunityID: cid, HostUserID: hostID})
	if err := svc.Repo.UpdateStatus(context.Background(), l.ID, lobbies.StatusClosed); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := svc.Send(context.Background(), lobbies.SendInput{
		LobbyID:      l.ID,
		AuthorKind:   lobbies.AuthorGuest,
		BodyMarkdown: "anyone there?",
	})
	if err != lobbies.ErrClosedOrExpired {
		t.Fatalf("want ErrClosedOrExpired, got %v", err)
	}
}

func TestSend_RejectsExpiredLobby(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	past := -1 * time.Second
	l, err := svc.Mint(context.Background(), lobbies.MintInput{
		CommunityID: cid, HostUserID: hostID, ExpiresIn: past,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = svc.Send(context.Background(), lobbies.SendInput{
		LobbyID: l.ID, AuthorKind: lobbies.AuthorGuest, BodyMarkdown: "hi",
	})
	if err != lobbies.ErrClosedOrExpired {
		t.Fatalf("want ErrClosedOrExpired for expired lobby, got %v", err)
	}
}

func TestSend_EmptyBodyRejected(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{CommunityID: cid, HostUserID: hostID})
	_, err := svc.Send(context.Background(), lobbies.SendInput{
		LobbyID: l.ID, AuthorKind: lobbies.AuthorHost, BodyMarkdown: "   ",
	})
	if err != lobbies.ErrEmptyBody {
		t.Fatalf("want ErrEmptyBody, got %v", err)
	}
}

func TestJoin_StampsName(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{CommunityID: cid, HostUserID: hostID})
	got, err := svc.Join(context.Background(), l.GuestToken, "Casey")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got.GuestDisplayName != "Casey" {
		t.Fatalf("want Casey, got %q", got.GuestDisplayName)
	}
}

func TestJoin_Idempotent(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{
		CommunityID: cid, HostUserID: hostID, GuestName: "PreSet",
	})
	got, err := svc.Join(context.Background(), l.GuestToken, "PreSet")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if got.GuestDisplayName != "PreSet" {
		t.Fatalf("idempotent join changed name: %q", got.GuestDisplayName)
	}
}

func TestJoin_RejectsClosed(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{CommunityID: cid, HostUserID: hostID})
	_ = svc.Repo.UpdateStatus(context.Background(), l.ID, lobbies.StatusClosed)
	_, err := svc.Join(context.Background(), l.GuestToken, "Late")
	if err != lobbies.ErrClosedOrExpired {
		t.Fatalf("want ErrClosedOrExpired, got %v", err)
	}
}

func TestPromote_NeedsEmail(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, _ := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{CommunityID: cid, HostUserID: hostID})
	_, err := svc.Promote(context.Background(), l.ID)
	if err != lobbies.ErrPromoteNeedsEmail {
		t.Fatalf("want ErrPromoteNeedsEmail, got %v", err)
	}
}

func TestPromote_IssuesInviteWhenEmailPresent(t *testing.T) {
	t.Parallel()
	svc, cid, hostID, inv := setupService(t)
	l, _ := svc.Mint(context.Background(), lobbies.MintInput{
		CommunityID: cid, HostUserID: hostID, GuestEmail: "guest@x.test",
	})
	code, err := svc.Promote(context.Background(), l.ID)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if code != "FAKE-CODE-XYZ" {
		t.Errorf("want fake code, got %q", code)
	}
	if inv.called != 1 || inv.lastCommunity != cid {
		t.Errorf("invite issuer not called correctly: %+v", inv)
	}
}
