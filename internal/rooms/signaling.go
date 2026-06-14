package rooms

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// signalIn is the wire shape posted to /rooms/{id}/signal/send.
type signalIn struct {
	To      string `json:"to"`      // recipient participant key
	Kind    string `json:"kind"`    // offer | answer | ice | bye
	Payload string `json:"payload"` // opaque JSON (SDP / ICE)
}

func (s *Service) RouteSignal(roomID, fromKey string, raw []byte) error {
	var in signalIn
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	switch in.Kind {
	case "offer", "answer", "ice", "bye":
	default:
		return errors.New("unknown signal kind")
	}
	if in.To == "" {
		return errors.New("missing recipient")
	}
	if !s.State.IsMember(roomID, fromKey) {
		return ErrNotMember
	}
	if !s.State.IsMember(roomID, in.To) {
		return errors.New("recipient not in room")
	}
	s.Bus.SendSignal(roomID, in.To, SignalEnvelope{
		FromKey: fromKey,
		Kind:    in.Kind,
		Payload: in.Payload,
	})
	return nil
}

// streamSignal is a raw SSE relay for the caller's signaling mailbox.
// Each envelope is written as `event: sig\ndata: <json>\n\n`.
func (s *Service) streamSignal(w http.ResponseWriter, r *http.Request, roomID, fromKey string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	inbox, unsub := s.Bus.SubscribeSignal(roomID, fromKey)
	defer unsub()

	// Initial hello so the client knows the stream is open.
	hello, _ := json.Marshal(map[string]string{"kind": "hello", "you": fromKey})
	writeSig(w, hello)
	flusher.Flush()

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			// SSE keep-alive comment.
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		case env, ok := <-inbox:
			if !ok {
				return
			}
			b, err := json.Marshal(map[string]string{
				"kind":    env.Kind,
				"from":    env.FromKey,
				"payload": env.Payload,
			})
			if err != nil {
				continue
			}
			writeSig(w, b)
			flusher.Flush()
		}
	}
}

func writeSig(w http.ResponseWriter, data []byte) {
	_, _ = w.Write([]byte("event: sig\ndata: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}
