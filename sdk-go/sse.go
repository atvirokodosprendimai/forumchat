package connector

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// The stream's bytes come from the server, which a worker does not fully
// control, so the parser caps how much it will buffer: an over-long line or an
// over-large accumulated frame ends the stream with an error rather than growing
// memory unbounded. The caps are generous next to a real chat message (one frame
// is one JSON line; even with many attachments it's far under 1 MiB).
const (
	maxLineBytes  = 1 << 20 // 1 MiB cap on a single SSE line
	maxFrameBytes = 1 << 20 // 1 MiB cap on one frame's accumulated data payload
)

// ErrFrameTooLarge ends the stream when the server sends a frame whose data
// exceeds maxFrameBytes (an over-long single line surfaces instead as
// bufio.ErrTooLong from the scanner). Exported so a caller's reconnect logic can
// distinguish "the peer is misbehaving" from an ordinary transport drop.
var ErrFrameTooLarge = errors.New("connector: SSE frame exceeds size limit")

// sseFrame is one decoded Server-Sent Events frame: an event name (defaulting to
// "message" per the SSE spec when the stream omits it) and its data payload.
type sseFrame struct {
	event string
	data  []byte
}

// scanSSE reads text/event-stream from r and calls onFrame once per complete
// frame (terminated by a blank line). It returns the error that ended the
// stream: nil on a clean server close, ErrFrameTooLarge / bufio.ErrTooLong if
// the server breaches a size cap, or the transport / ctx-driven read error
// otherwise. Returning the cause lets the caller decide whether to reconnect.
//
// It parses only the subset of SSE the connector stream emits: `event:` /
// `data:` field lines, `:`-prefixed comment heartbeats (ignored), and the
// blank-line dispatch — the server never sends id:/retry:.
func scanSSE(r io.Reader, onFrame func(sseFrame) bool) error {
	sc := bufio.NewScanner(r)
	// Bound per-line memory: the buffer grows on demand up to maxLineBytes; a line
	// longer than that makes Scan stop with bufio.ErrTooLong (returned via Err).
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	var (
		event   string
		data    strings.Builder
		hasData bool
	)
	// dispatch flushes the buffered frame to the callback and resets accumulators.
	// It returns whether scanning should continue.
	dispatch := func() bool {
		if event == "" && !hasData {
			return true // stray blank line between frames — nothing buffered
		}
		name := event
		if name == "" {
			name = "message" // SSE default when the server omits the event field
		}
		f := sseFrame{event: name, data: []byte(data.String())}
		event, hasData = "", false
		data.Reset()
		return onFrame(f)
	}

	for sc.Scan() {
		// ScanLines strips the trailing \n and any \r, so CRLF and LF both work.
		switch line := sc.Text(); {
		case line == "":
			if !dispatch() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
			// Comment line (the server's `:\n\n` heartbeat). Ignore.
		default:
			switch field, value := splitField(line); field {
			case "event":
				event = value
			case "data":
				// Cap the running frame so many small data: lines can't accumulate
				// without bound (the +1 accounts for the joining \n).
				if data.Len()+len(value)+1 > maxFrameBytes {
					return ErrFrameTooLarge
				}
				if hasData {
					data.WriteByte('\n') // SSE joins multiple data: lines with \n
				}
				data.WriteString(value)
				hasData = true
			}
			// Unknown fields (id, retry, …) are intentionally ignored.
		}
	}
	if err := sc.Err(); err != nil {
		return err // transport error, or bufio.ErrTooLong for an over-long line
	}
	// Clean EOF: flush a frame the server left un-terminated (no trailing blank
	// line) so the last message isn't lost. Safe because EOF here is a clean read
	// end, not a mid-frame transport failure.
	if hasData {
		dispatch()
	}
	return nil
}

// splitField splits an SSE field line "name: value" into its name and value,
// stripping exactly one optional leading space from the value (per the SSE
// spec). A line with no colon is a field name with an empty value.
func splitField(line string) (field, value string) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimPrefix(line[i+1:], " ")
}
