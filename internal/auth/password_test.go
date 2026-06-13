package auth

import "testing"

func TestPasswordHashRoundtrip(t *testing.T) {
	t.Parallel()
	pw := "correct horse battery staple"
	h, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !CheckPassword(h, pw) {
		t.Fatal("CheckPassword=false for correct password")
	}
	if CheckPassword(h, pw+"x") {
		t.Fatal("CheckPassword=true for wrong password")
	}
}

func TestPasswordTooShort(t *testing.T) {
	t.Parallel()
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("expected error for short password")
	}
}
