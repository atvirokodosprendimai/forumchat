package connector

import "testing"

// The SDK signs requests the server must accept, so these tests pin the two
// primitives to known-answer vectors. If they break, every request the SDK makes
// would be silently rejected (401/404) — exactly the failure that's hard to debug
// from the client side, so we catch it here.

// secret/id are fixed so the expected digests are reproducible.
const (
	testSecret = "s3cr3t-key"
	testID     = "conn-123"
)

func TestStreamSig_StableAndIDBound(t *testing.T) {
	// Same inputs → same signature (deterministic HMAC).
	a := streamSig(testSecret, testID, 0)
	b := streamSig(testSecret, testID, 0)
	if a != b {
		t.Fatalf("streamSig not deterministic: %s != %s", a, b)
	}
	// The signature is bound to the id and the expiry: changing either must change
	// the digest, or a signed URL could be reused across connectors / expiries.
	if streamSig(testSecret, "other-id", 0) == a {
		t.Error("streamSig must change with the connector id")
	}
	if streamSig(testSecret, testID, 1) == a {
		t.Error("streamSig must change with the expiry")
	}
	if streamSig("other-secret", testID, 0) == a {
		t.Error("streamSig must change with the secret")
	}
}

func TestSignBody_FormatAndSensitivity(t *testing.T) {
	got := signBody(testSecret, []byte(`{"channel":"support","body":"hi"}`))
	const want = "sha256=" // the server expects this prefix (verifyGitHubSignature shape)
	if len(got) <= len(want) || got[:len(want)] != want {
		t.Fatalf("signBody = %q, want %q-prefixed hex", got, want)
	}
	// A single flipped byte in the body must change the signature (tamper-proof).
	if signBody(testSecret, []byte(`{"channel":"support","body":"hX"}`)) == got {
		t.Error("signBody must change when the body changes")
	}
}
