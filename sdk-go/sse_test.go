package connector

import (
	"strings"
	"testing"
)

// scanSSE decodes bytes the server controls, so these tests pin the framing
// rules: event/data pairing, the `:` heartbeat skip, multi-line data joining, a
// final frame without a trailing blank line, and the size caps that defend
// against a misbehaving server.
func TestScanSSE_FramesHeartbeatsAndTail(t *testing.T) {
	// A realistic stream: handshake, a heartbeat comment, a message, then a final
	// message the server flushes WITHOUT a trailing blank line (clean EOF).
	raw := "event: ready\ndata: {\"nick\":\"Acme\"}\n\n" +
		":\n\n" + // heartbeat — must be ignored, must not emit a frame
		"event: message\ndata: {\"id\":\"1\"}\n\n" +
		"event: message\ndata: line1\ndata: line2\n" // no trailing blank line

	type got struct {
		event string
		data  string
	}
	var frames []got
	err := scanSSE(strings.NewReader(raw), func(f sseFrame) bool {
		frames = append(frames, got{f.event, string(f.data)})
		return true
	})
	if err != nil {
		t.Fatalf("scanSSE err = %v, want nil (clean EOF)", err)
	}
	want := []got{
		{"ready", `{"nick":"Acme"}`},
		{"message", `{"id":"1"}`},
		{"message", "line1\nline2"}, // multiple data: lines join with \n
	}
	if len(frames) != len(want) {
		t.Fatalf("got %d frames %+v, want %d", len(frames), frames, len(want))
	}
	for i := range want {
		if frames[i] != want[i] {
			t.Errorf("frame %d = %+v, want %+v", i, frames[i], want[i])
		}
	}
}

func TestScanSSE_CallbackStopsScan(t *testing.T) {
	// A callback returning false ends scanning cleanly (nil), even with more data
	// buffered — the contract Stream relies on for graceful teardown.
	raw := "event: message\ndata: a\n\nevent: message\ndata: b\n\n"
	n := 0
	err := scanSSE(strings.NewReader(raw), func(sseFrame) bool {
		n++
		return false // stop after the first frame
	})
	if err != nil {
		t.Fatalf("scanSSE err = %v, want nil", err)
	}
	if n != 1 {
		t.Errorf("callback ran %d times, want 1", n)
	}
}

// TestScanSSE_FrameTooLarge proves the per-frame cap stops an unbounded
// accumulation of data: lines (a server-driven memory-growth vector Codex
// flagged) instead of buffering it all.
func TestScanSSE_FrameTooLarge(t *testing.T) {
	var b strings.Builder
	// Enough 1 KiB data: lines that the accumulated PAYLOAD (not the wire size)
	// overshoots maxFrameBytes, with no blank line to flush — so the cap, not
	// memory, must end the scan.
	val := strings.Repeat("x", 1024)
	for i := 0; i < maxFrameBytes/1024+50; i++ {
		b.WriteString("data: " + val + "\n")
	}
	err := scanSSE(strings.NewReader(b.String()), func(sseFrame) bool { return true })
	if err != ErrFrameTooLarge {
		t.Fatalf("scanSSE err = %v, want ErrFrameTooLarge", err)
	}
}
