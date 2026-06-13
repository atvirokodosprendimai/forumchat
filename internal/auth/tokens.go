package auth

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

func RandomToken(byteLen int) (string, error) {
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return strings.ToLower(s), nil
}

func InviteCodeText() (string, error) {
	t, err := RandomToken(10)
	if err != nil {
		return "", err
	}
	return strings.ToUpper(t), nil
}
