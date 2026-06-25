package connectors_test

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/atvirokodosprendimai/forumchat/internal/auth"
	"github.com/atvirokodosprendimai/forumchat/internal/chat"
	"github.com/atvirokodosprendimai/forumchat/internal/community"
	"github.com/atvirokodosprendimai/forumchat/internal/connectors"
	"github.com/atvirokodosprendimai/forumchat/internal/storage/sqlite"
)

// harness wires a connectors.Handler against a temp DB with a live chat Bus, the
// same way main.go does, so the stream's in-process fan-out works in tests.
type harness struct {
	h       *connectors.Handler
	svc     *connectors.Service
	chatSvc *chat.Service
	bus     *chat.Bus
	srv     *httptest.Server
	comm    community.Community
	general chat.Channel
	authSvc *auth.Service
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.Migrate(ctx, db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	c, err := community.NewRepo(db).BootstrapOrFetch(ctx, "test", "Test")
	if err != nil {
		t.Fatalf("community: %v", err)
	}
	chatRepo := chat.NewRepo(db)
	chatSvc := chat.NewService(chatRepo)
	general, err := chatRepo.EnsureDefaultChannel(ctx, c.ID)
	if err != nil {
		t.Fatalf("default channel: %v", err)
	}
	bus := chat.NewBus()
	authSvc := &auth.Service{Repo: auth.NewRepo(db)}
	connRepo := connectors.NewRepo(db)
	connSvc := connectors.NewService(connRepo, authSvc, chatRepo)
	h := &connectors.Handler{
		Repo: connRepo, Svc: connSvc, Chat: chatSvc, ChatRepo: chatRepo,
		Bus: bus, NewMsgBus: chat.NewBus(), Log: slog.Default(),
		// Minimal delete seam (soft-delete only) so the allowlist gate — which
		// runs in the handler BEFORE the seam — can be exercised.
		DeleteMessage: func(ctx context.Context, _ string, msgID, _ string) error {
			return chatRepo.SoftDelete(ctx, msgID)
		},
	}
	r := chi.NewRouter()
	r.Get("/bots/{id}/stream", h.GetStream)
	r.Post("/bots/{id}/send", h.PostSend)
	r.Post("/bots/{id}/delete", h.PostDeleteMessage)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &harness{h: h, svc: connSvc, chatSvc: chatSvc, bus: bus, srv: srv, comm: c, general: general, authSvc: authSvc}
}

// memberSay creates a real member (synthetic) and posts a chat message as them,
// then fans out on the bus — a stand-in for a human typing in the channel.
func (hz *harness) memberSay(t *testing.T, nick, body string) (userID, msgID string) {
	t.Helper()
	ctx := context.Background()
	uid, err := hz.authSvc.CreateServiceAccount(ctx, hz.comm.ID, nick, "")
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	m, err := hz.chatSvc.Send(ctx, chat.SendInput{CommunityID: hz.comm.ID, ChannelID: hz.general.ID, AuthorID: uid, BodyMarkdown: body})
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	hz.bus.Broadcast(hz.general.ID)
	return uid, m.ID
}

func TestPostSendAuth(t *testing.T) {
	t.Parallel()
	hz := newHarness(t)
	conn, err := hz.svc.Create(context.Background(), connectors.CreateInput{
		CommunityID: hz.comm.ID, Name: "Acme", Capabilities: []string{"send"},
		ChannelIDs: []string{hz.general.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	body := []byte(`{"channel":"general","body":"hello from the worker"}`)

	// Unknown id → 404.
	if code := hz.post(t, "does-not-exist", body, connectors.SignBody(conn.Secret, body)); code != http.StatusNotFound {
		t.Fatalf("unknown id: got %d want 404", code)
	}
	// Bad signature → 401.
	if code := hz.post(t, conn.ID, body, "sha256=deadbeef"); code != http.StatusUnauthorized {
		t.Fatalf("bad sig: got %d want 401", code)
	}
	// Valid signed send → 200 and a real human message authored by the member.
	if code := hz.post(t, conn.ID, body, connectors.SignBody(conn.Secret, body)); code != http.StatusOK {
		t.Fatalf("valid send: got %d want 200", code)
	}
	recent, err := hz.h.ChatRepo.Recent(context.Background(), hz.general.ID, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	var found bool
	for _, m := range recent {
		if m.BodyMarkdown == "hello from the worker" {
			found = true
			if m.Kind != chat.KindUser || m.AuthorID == nil || *m.AuthorID != conn.UserID {
				t.Fatalf("send not authored as the member: kind=%s author=%v", m.Kind, m.AuthorID)
			}
		}
	}
	if !found {
		t.Fatal("connector send did not land in the channel")
	}
}

func TestPostSendForeignChannelRejected(t *testing.T) {
	t.Parallel()
	hz := newHarness(t)
	// Connector allowlisted to a NON-general channel; sending to general → 403.
	creator, err := hz.authSvc.CreateServiceAccount(context.Background(), hz.comm.ID, "creator", "")
	if err != nil {
		t.Fatalf("creator: %v", err)
	}
	other, err := hz.chatSvc.CreateChannel(context.Background(), hz.comm.ID, creator, "ops", "")
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	conn, err := hz.svc.Create(context.Background(), connectors.CreateInput{
		CommunityID: hz.comm.ID, Name: "Acme", Capabilities: []string{"send"},
		ChannelIDs: []string{other.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	body := []byte(`{"channel":"general","body":"sneaky"}`)
	if code := hz.post(t, conn.ID, body, connectors.SignBody(conn.Secret, body)); code != http.StatusForbidden {
		t.Fatalf("foreign channel: got %d want 403", code)
	}
}

func TestStreamDeliversSkipsOwn(t *testing.T) {
	t.Parallel()
	hz := newHarness(t)
	conn, err := hz.svc.Create(context.Background(), connectors.CreateInput{
		CommunityID: hz.comm.ID, Name: "Acme", Capabilities: []string{"send"},
		ChannelIDs: []string{hz.general.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	frames := hz.openStream(t, conn)
	if ev := frames.next(t); ev.event != "ready" {
		t.Fatalf("first frame = %q, want ready", ev.event)
	}
	// Live-only connect (no cursor, no params): the boundary marker follows the
	// handshake immediately, with since=0 (no backlog replayed).
	if ev := frames.next(t); ev.event != "live" {
		t.Fatalf("second frame = %q, want live", ev.event)
	} else if !strings.Contains(ev.data, `"since":0`) {
		t.Fatalf("live frame should report no backlog, got: %s", ev.data)
	}

	// The connector's OWN message must not echo back. Post it, then post a human
	// message; the next delivered frame must be the human one, proving the own
	// message was skipped (deterministic — no timeout guessing).
	ownBody := []byte(`{"channel":"general","body":"i am the bot"}`)
	if code := hz.post(t, conn.ID, ownBody, connectors.SignBody(conn.Secret, ownBody)); code != http.StatusOK {
		t.Fatalf("own send: %d", code)
	}
	hz.memberSay(t, "alice", "hi @Acme can you help")

	ev := frames.next(t)
	if ev.event != "message" {
		t.Fatalf("frame = %q, want message", ev.event)
	}
	if !strings.Contains(ev.data, `"nick":"alice"`) || !strings.Contains(ev.data, `"mentioned":true`) {
		t.Fatalf("unexpected message frame: %s", ev.data)
	}
	if strings.Contains(ev.data, "i am the bot") {
		t.Fatalf("connector echoed its own message: %s", ev.data)
	}
}

// TestStreamCatchUpReplaysBacklog proves the missed-message replay: a human
// message posted BEFORE the worker connects is delivered as backlog when the
// stream carries a ?since= older than it, then the `live` marker follows — and a
// plain (live-only) connect does NOT replay it.
func TestStreamCatchUpReplaysBacklog(t *testing.T) {
	t.Parallel()
	hz := newHarness(t)
	conn, err := hz.svc.Create(context.Background(), connectors.CreateInput{
		CommunityID: hz.comm.ID, Name: "Acme", Capabilities: []string{"send"},
		ChannelIDs: []string{hz.general.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A human types while no worker is attached.
	since := time.Now().Add(-time.Second) // a watermark just before the message
	hz.memberSay(t, "alice", "you missed this one")

	// Catch-up connect: ready → backlog message → live.
	cu := hz.openStreamSince(t, conn, since)
	if ev := cu.next(t); ev.event != "ready" {
		t.Fatalf("catch-up first frame = %q, want ready", ev.event)
	}
	ev := cu.next(t)
	if ev.event != "message" || !strings.Contains(ev.data, "you missed this one") {
		t.Fatalf("catch-up frame = %q/%s, want the backlog message", ev.event, ev.data)
	}
	if ev := cu.next(t); ev.event != "live" {
		t.Fatalf("after backlog, frame = %q, want live", ev.event)
	} else if strings.Contains(ev.data, `"since":0`) {
		t.Fatalf("catch-up live marker should carry a non-zero since, got: %s", ev.data)
	}

	// Control: a live-only connect (no ?since=) must NOT replay the same message —
	// ready is followed straight by the live marker.
	live := hz.openStream(t, conn)
	if ev := live.next(t); ev.event != "ready" {
		t.Fatalf("live-only first frame = %q, want ready", ev.event)
	}
	if ev := live.next(t); ev.event != "live" {
		t.Fatalf("live-only second frame = %q, want live (no backlog)", ev.event)
	}
}

func TestDeleteRespectsChannelAllowlist(t *testing.T) {
	t.Parallel()
	hz := newHarness(t)
	// Connector scoped to a NON-general channel, granted delete.
	creator, err := hz.authSvc.CreateServiceAccount(context.Background(), hz.comm.ID, "creator", "")
	if err != nil {
		t.Fatalf("creator: %v", err)
	}
	ops, err := hz.chatSvc.CreateChannel(context.Background(), hz.comm.ID, creator, "ops", "")
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	conn, err := hz.svc.Create(context.Background(), connectors.CreateInput{
		CommunityID: hz.comm.ID, Name: "Mod", Capabilities: []string{"send", "delete"},
		ChannelIDs: []string{ops.ID},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A message in #general (OUTSIDE the connector's allowlist).
	_, msgID := hz.memberSay(t, "alice", "hi")

	body := []byte(`{"message_id":"` + msgID + `"}`)
	code := hz.postTo(t, conn.ID, "delete", body, connectors.SignBody(conn.Secret, body))
	if code != http.StatusForbidden {
		t.Fatalf("delete outside allowlist: got %d want 403", code)
	}
}

func TestCreateRejectsAllInvalidChannels(t *testing.T) {
	t.Parallel()
	hz := newHarness(t)
	_, err := hz.svc.Create(context.Background(), connectors.CreateInput{
		CommunityID: hz.comm.ID, Name: "X", Capabilities: []string{"send"},
		ChannelIDs: []string{"forged-1", "forged-2"}, // none real → must NOT collapse to all
	})
	if err == nil {
		t.Fatal("expected ErrUnknownChannels for an all-invalid channel set, got nil")
	}
}

// ----- small SSE client + post helpers ---------------------------------------

func (hz *harness) post(t *testing.T, id string, body []byte, sig string) int {
	return hz.postTo(t, id, "send", body, sig)
}

func (hz *harness) postTo(t *testing.T, id, action string, body []byte, sig string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, hz.srv.URL+"/bots/"+id+"/"+action, bytes.NewReader(body))
	req.Header.Set("X-Signature", sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type sseFrame struct{ event, data string }

type sseReader struct {
	cancel context.CancelFunc
	lines  *bufio.Reader
	body   io.Closer
}

func (hz *harness) openStream(t *testing.T, conn connectors.Connector) *sseReader {
	return hz.openStreamRaw(t, conn, "")
}

// openStreamSince opens the stream with a ?since= catch-up watermark.
func (hz *harness) openStreamSince(t *testing.T, conn connectors.Connector, since time.Time) *sseReader {
	return hz.openStreamRaw(t, conn, "&since="+strconv.FormatInt(since.Unix(), 10))
}

func (hz *harness) openStreamRaw(t *testing.T, conn connectors.Connector, extra string) *sseReader {
	t.Helper()
	url := hz.srv.URL + "/bots/" + conn.ID + "/stream?exp=0&sig=" + connectors.StreamSig(conn.Secret, conn.ID, 0) + extra
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("stream status %d", resp.StatusCode)
	}
	t.Cleanup(cancel)
	return &sseReader{cancel: cancel, lines: bufio.NewReader(resp.Body), body: resp.Body}
}

// next reads the next non-heartbeat SSE frame (event + data lines), failing on a
// deadline so a missed message can't hang the test forever.
func (s *sseReader) next(t *testing.T) sseFrame {
	t.Helper()
	type res struct {
		f  sseFrame
		ok bool
	}
	ch := make(chan res, 1)
	go func() {
		var f sseFrame
		for {
			line, err := s.lines.ReadString('\n')
			if err != nil {
				ch <- res{}
				return
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "event: "):
				f.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				f.data = strings.TrimPrefix(line, "data: ")
			case line == "" && f.event != "":
				ch <- res{f, true}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		if !r.ok {
			t.Fatal("stream closed before a frame arrived")
		}
		return r.f
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for an SSE frame")
		return sseFrame{}
	}
}
