// Package secretbox seals and opens short secrets (per-community API keys, S3
// credentials) for storage at rest. With a 32-byte key it uses AES-256-GCM;
// with no key it is a tagged passthrough so local dev works without SECRETS_KEY.
// Open transparently handles both encodings AND bare legacy plaintext, so
// enabling a key later still reads pre-existing rows.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	encPrefix   = "enc:v1:"
	plainPrefix = "plain:"
)

// Box seals and opens secrets. The zero value is a valid passthrough Box.
type Box struct{ gcm cipher.AEAD }

// New builds a Box. An empty key yields a passthrough Box (dev mode). A
// non-empty key must be exactly 32 bytes (AES-256); any other length errors.
func New(key string) (*Box, error) {
	if key == "" {
		return &Box{}, nil
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secretbox: SECRETS_KEY must be exactly 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{gcm: gcm}, nil
}

// Enabled reports whether the Box encrypts (true) or is a dev passthrough (false).
func (b *Box) Enabled() bool { return b != nil && b.gcm != nil }

// Seal returns a storable string for plaintext. Empty input returns "" so an
// unset secret stays unset rather than becoming an encrypted empty string.
func (b *Box) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if !b.Enabled() {
		return plainPrefix + plaintext, nil
	}
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := b.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(ct), nil
}

// Open reverses Seal. It accepts encrypted ("enc:v1:") and passthrough
// ("plain:") values, and treats anything else as bare legacy plaintext.
func (b *Box) Open(stored string) (string, error) {
	switch {
	case stored == "":
		return "", nil
	case strings.HasPrefix(stored, plainPrefix):
		return strings.TrimPrefix(stored, plainPrefix), nil
	case strings.HasPrefix(stored, encPrefix):
		if !b.Enabled() {
			return "", errors.New("secretbox: encrypted value but no SECRETS_KEY configured")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
		if err != nil {
			return "", err
		}
		ns := b.gcm.NonceSize()
		if len(raw) < ns {
			return "", errors.New("secretbox: ciphertext too short")
		}
		pt, err := b.gcm.Open(nil, raw[:ns], raw[ns:], nil)
		if err != nil {
			return "", err
		}
		return string(pt), nil
	default:
		return stored, nil // bare legacy plaintext
	}
}
