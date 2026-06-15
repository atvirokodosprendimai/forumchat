package mailbox

import "testing"

func TestCursorRoundtrip(t *testing.T) {
	cases := []*QueueCursor{
		{ReceivedAtUnixMS: 1734267600123, ID: "01HXYZ-deadbeef"},
		{ReceivedAtUnixMS: 0, ID: "z"},
		{ReceivedAtUnixMS: 1, ID: "with:colon:in:id"},
	}
	for _, c := range cases {
		got, err := decodeCursor(encodeCursor(c))
		if err != nil {
			t.Fatalf("decode %+v: %v", c, err)
		}
		if got.ReceivedAtUnixMS != c.ReceivedAtUnixMS || got.ID != c.ID {
			t.Fatalf("roundtrip mismatch: want %+v got %+v", c, got)
		}
	}
}

func TestCursorEmpty(t *testing.T) {
	if got, err := decodeCursor(""); err != nil || got != nil {
		t.Fatalf("empty cursor should decode to nil, got %v err=%v", got, err)
	}
	if encodeCursor(nil) != "" {
		t.Fatalf("nil cursor should encode to empty string")
	}
}

func TestCursorBadInput(t *testing.T) {
	if _, err := decodeCursor("!!!not-base64!!!"); err == nil {
		t.Fatalf("bad base64 should error")
	}
	if _, err := decodeCursor("aGVsbG8"); err == nil {
		// "hello" — no colon
		t.Fatalf("missing colon should error")
	}
	if _, err := decodeCursor("bm90X2FfbnVtYmVyOmlk"); err == nil {
		// "not_a_number:id"
		t.Fatalf("bad ms should error")
	}
}
