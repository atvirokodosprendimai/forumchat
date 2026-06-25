// Command tinychat is a tiny terminal chat client built on the forumchat
// connector SDK. It joins a community as an external connector: it opens the
// signed SSE stream to print messages live, and sends whatever you type back
// into the channel as the connector's (human-looking) member.
//
// Get the three values from the community admin page
// (/c/{slug}/admin/connectors, with CONNECTORS_ENABLED=true) — they're revealed
// once on create. Then:
//
//	go run . \
//	  -base https://chat.example.com \
//	  -id   <connector id> \
//	  -secret <connector secret> \
//	  -channel support        # optional; omit to use the connector's sole channel
//
// Flags fall back to the env vars BASE_URL, CONNECTOR_ID, CONNECTOR_SECRET,
// CHANNEL. Type a line and press Enter to send; "/quit" or Ctrl-C exits. Set
// NO_COLOR to disable ANSI colour.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	connector "github.com/atvirokodosprendimai/forumchat/sdk-go"
)

func main() {
	// Config: flags win, else env, so the same binary works from a shell script
	// or a .env-style setup.
	base := flag.String("base", env("BASE_URL", "http://localhost:8080"), "forumchat base URL")
	id := flag.String("id", env("CONNECTOR_ID", ""), "connector id")
	secret := flag.String("secret", env("CONNECTOR_SECRET", ""), "connector secret")
	channel := flag.String("channel", env("CHANNEL", ""), "channel slug to send to (default: the connector's sole channel)")
	flag.Parse()

	if *id == "" || *secret == "" {
		fmt.Fprintln(os.Stderr, "tinychat: -id and -secret are required (see admin → connectors). Run with -h for help.")
		os.Exit(2)
	}

	// One ctx cancelled by Ctrl-C / SIGTERM, threaded through the stream and the
	// signed sends so everything tears down together.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a := &app{
		client:  connector.New(*base, *id, *secret),
		channel: *channel,
		color:   os.Getenv("NO_COLOR") == "",
	}

	// Reader: the signed SSE stream, with reconnect-and-backoff. The SDK's Stream
	// is one-shot by design (it doesn't hide reconnects), so the policy lives
	// here — the canonical pattern for a long-lived worker.
	go a.streamLoop(ctx)

	// Writer: stdin → Send. Runs in its own goroutine so Ctrl-C stays responsive
	// even while a blocking Read waits on the terminal.
	go a.inputLoop(ctx, stop)

	<-ctx.Done()
	a.line(a.dim("— disconnected, bye —"))
}

// app holds the shared client and the small mutable UI state (the active send
// channel, learned from the ready handshake when no -channel was given).
type app struct {
	client *connector.Client
	color  bool

	mu      sync.Mutex
	channel string // guarded: read by inputLoop, written by the ready handler
}

// streamLoop keeps the read stream open, reconnecting with capped exponential
// backoff until ctx is cancelled. A clean close (Stream returns nil) or any
// error both trigger a retry — a chat worker wants to stay attached.
func (a *app) streamLoop(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		a.line(a.dim("connecting…"))
		// exp=0 → a non-expiring signed URL; the SDK builds + signs it for us.
		err := a.client.Stream(ctx, connector.Handlers{
			OnReady:   a.onReady,
			OnMessage: a.onMessage,
		}, 0)
		switch {
		case ctx.Err() != nil:
			return // caller asked us to stop; not a failure
		case err != nil:
			a.line(a.dim(fmt.Sprintf("✗ stream ended: %v — retrying in %s", err, backoff)))
		default:
			a.line(a.dim(fmt.Sprintf("stream closed — retrying in %s", backoff)))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// onReady prints the handshake banner and, when no channel was configured,
// adopts the connector's first subscribed channel as the send target so typing
// Just Works for the common single-channel connector.
func (a *app) onReady(r connector.Ready) {
	slugs := make([]string, 0, len(r.Channels))
	for _, ch := range r.Channels {
		slugs = append(slugs, "#"+ch.Slug)
	}
	a.mu.Lock()
	if a.channel == "" && len(r.Channels) > 0 {
		a.channel = r.Channels[0].Slug
	}
	target := a.channel
	a.mu.Unlock()

	a.line(a.bold(a.green("● connected as "+r.Nick)) +
		a.dim(" — channels: "+strings.Join(slugs, " ")))
	if target != "" {
		a.line(a.dim("typing sends to #" + target + " — type a message, or /quit"))
	}
}

// onMessage renders one incoming message: dim timestamp, a stable per-nick
// colour, and an inverse highlight when the message @mentions this connector.
func (a *app) onMessage(e connector.Event) {
	ts := ""
	if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
		ts = a.dim(t.Local().Format("15:04"))
	}
	nick := a.nickColor(e.Nick, "@"+e.Nick)
	body := e.BodyMD
	if e.Mentioned {
		// Make a mention impossible to miss in a busy stream.
		body = a.invert(" " + body + " ")
	}
	loc := a.dim("#" + e.Channel)
	a.line(fmt.Sprintf("%s %s %s %s", ts, loc, nick, body))
}

// inputLoop reads stdin lines and sends each as a chat message. "/quit" stops
// the program. Own sends are echoed locally because the stream never echoes the
// connector's own messages back (no double-print).
func (a *app) inputLoop(ctx context.Context, stop context.CancelFunc) {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		if text == "/quit" {
			stop()
			return
		}
		a.mu.Lock()
		target := a.channel
		a.mu.Unlock()

		res, err := a.client.Send(ctx, target, text)
		if err != nil {
			a.line(a.dim("✗ send failed: " + err.Error()))
			continue
		}
		// Echo our own line (the stream suppresses it) so the conversation reads
		// in order locally.
		a.line(a.dim(time.Now().Format("15:04")) + " " +
			a.dim("#"+res.Channel) + " " + a.bold(a.green("you")) + " " + text)
	}
}

// ---- terminal rendering helpers ---------------------------------------------
//
// Kept stdlib-only: ANSI escapes, gated by the color flag (NO_COLOR), so the
// output is still readable when piped to a file or a colour-blind terminal.

// nickPalette is a small set of readable foreground colours; nicks hash into it
// so the same person keeps the same colour across messages.
var nickPalette = []string{"\x1b[36m", "\x1b[35m", "\x1b[33m", "\x1b[32m", "\x1b[34m", "\x1b[31m", "\x1b[96m", "\x1b[95m"}

func (a *app) nickColor(seed, s string) string {
	if !a.color {
		return s
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(seed))
	c := nickPalette[h.Sum32()%uint32(len(nickPalette))]
	return c + "\x1b[1m" + s + "\x1b[0m"
}

func (a *app) wrap(code, s string) string {
	if !a.color {
		return s
	}
	return code + s + "\x1b[0m"
}

func (a *app) dim(s string) string    { return a.wrap("\x1b[2m", s) }
func (a *app) bold(s string) string   { return a.wrap("\x1b[1m", s) }
func (a *app) green(s string) string  { return a.wrap("\x1b[32m", s) }
func (a *app) invert(s string) string { return a.wrap("\x1b[7m", s) }

// line prints one rendered line to stdout. Centralised so every print goes
// through the same path (easy to redirect or add locking later).
func (a *app) line(s string) { fmt.Println(s) }

// env returns the value of key, or def when unset/empty.
func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
