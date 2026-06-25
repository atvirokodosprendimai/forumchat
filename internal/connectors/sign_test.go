package connectors

import (
	"testing"
	"time"
)

func TestStreamSigRoundTrip(t *testing.T) {
	const secret, id = "s3cr3t-key", "conn-123"
	now := time.Unix(1_000_000, 0)

	// A non-expiring (exp=0) signature verifies.
	sig := StreamSig(secret, id, 0)
	if !VerifyStream(secret, id, sig, 0, now) {
		t.Fatal("valid non-expiring stream sig rejected")
	}

	// A tampered signature is rejected.
	if VerifyStream(secret, id, sig+"00", 0, now) {
		t.Fatal("tampered sig accepted")
	}
	// A different secret can't produce a valid sig.
	if VerifyStream("other-key", id, sig, 0, now) {
		t.Fatal("sig verified under wrong secret")
	}
	// The signature is bound to the id — it can't be replayed for another.
	if VerifyStream(secret, "conn-999", sig, 0, now) {
		t.Fatal("sig replayed onto a different connector id")
	}
}

func TestStreamSigExpiry(t *testing.T) {
	const secret, id = "k", "c"
	exp := int64(2_000)
	sig := StreamSig(secret, id, exp)

	if !VerifyStream(secret, id, sig, exp, time.Unix(1_999, 0)) {
		t.Fatal("unexpired sig rejected")
	}
	if VerifyStream(secret, id, sig, exp, time.Unix(2_001, 0)) {
		t.Fatal("expired sig accepted")
	}
}

func TestVerifyBody(t *testing.T) {
	const secret = "body-key"
	body := []byte(`{"channel":"general","body":"hi"}`)

	sig := SignBody(secret, body)
	if !VerifyBody(secret, body, sig) {
		t.Fatal("valid body signature rejected")
	}
	// A flipped body byte invalidates the signature.
	bad := append([]byte{}, body...)
	bad[0] = 'X'
	if VerifyBody(secret, bad, sig) {
		t.Fatal("signature verified against tampered body")
	}
	// Wrong secret, missing prefix, and garbage hex are all rejected.
	if VerifyBody("wrong", body, sig) {
		t.Fatal("verified under wrong secret")
	}
	if VerifyBody(secret, body, "deadbeef") {
		t.Fatal("missing sha256= prefix accepted")
	}
	if VerifyBody(secret, body, "sha256=zzzz") {
		t.Fatal("non-hex signature accepted")
	}
	if VerifyBody(secret, body, "") {
		t.Fatal("empty header accepted")
	}
}
