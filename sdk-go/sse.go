package connector

import (
	"bufio"
	"io"
	"strings"
)

// sseFrame is one decoded Server-Sent Events frame: an event name (defaulting to
// "message" per the SSE spec when the stream omits it) and its data payload.
type sseFrame struct {
	event string
	data  []byte
}

// scanSSE reads text/event-stream from r and calls onFrame once per complete
// frame (terminated by a blank line). It returns the error that ended the
// stream — io.EOF on a clean server close, or the transport / ctx-driven read
// error otherwise. Returning the cause (rather than swallowing it) lets the
// caller decide whether to reconnect.
//
// A bufio.Reader with ReadString is used rather than bufio.Scanner so a single
// large data line (a message with many attachments) can't trip Scanner's default
// 64 KiB token cap. The parser implements only the subset of SSE the connector
// stream emits: `event:` / `data:` field lines, `:`-prefixed comment heartbeats
// (ignored), and the blank-line dispatch — the server never sends id:/retry:.
func scanSSE(r io.Reader, onFrame func(sseFrame) bool) error {
	br := bufio.NewReader(r)
	var (
		event   string
		data    strings.Builder
		hasData bool
	)
	// dispatch flushes the buffered frame to the callback and resets the
	// accumulators. It returns whether scanning should continue.
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

	for {
		line, err := br.ReadString('\n')
		// Process whatever was read before handling a terminal error, so the final
		// frame isn't lost if the server closes without a trailing blank line.
		if line != "" {
			switch trimmed := strings.TrimRight(line, "\r\n"); {
			case trimmed == "":
				if !dispatch() {
					return nil
				}
			case strings.HasPrefix(trimmed, ":"):
				// Comment line (the server's `:\n\n` heartbeat). Ignore.
			default:
				switch field, value := splitField(trimmed); field {
				case "event":
					event = value
				case "data":
					if hasData {
						data.WriteByte('\n') // SSE joins multiple data: lines with \n
					}
					data.WriteString(value)
					hasData = true
				}
				// Unknown fields (id, retry, …) are intentionally ignored.
			}
		}
		if err != nil {
			// On a clean EOF, flush any frame the server left un-terminated (no
			// trailing blank line) so the last message isn't lost. Only on EOF: a
			// mid-frame transport error could be a truncated payload, which we'd
			// rather drop than hand the caller a partial. (In practice the server
			// always ends frames with \n\n, so this is belt-and-braces.)
			if err == io.EOF && hasData {
				dispatch()
			}
			return err
		}
	}
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
