// Package secretbox provides authenticated encryption for secrets stored at
// rest (LLM API keys, S3/R2 credentials). Values are encrypted with AES-256-GCM
// under a key derived from the OBSERVE_SECRET_KEY environment variable.
//
// Posture is fail-closed: encrypting without a configured master key is an
// error (so a secret is never silently written in plaintext), and decrypting a
// value that is marked encrypted when no key is configured is also an error.
// Plaintext values written before encryption was enabled decrypt as-is for
// backward compatibility.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

const prefix = "enc:v1:"

// ErrNoKey is returned when an operation needs the master key but
// OBSERVE_SECRET_KEY is not set.
var ErrNoKey = errors.New("secretbox: OBSERVE_SECRET_KEY not set")

// Configured reports whether a master key is available.
func Configured() bool { return os.Getenv("OBSERVE_SECRET_KEY") != "" }

func key() ([]byte, error) {
	raw := os.Getenv("OBSERVE_SECRET_KEY")
	if raw == "" {
		return nil, ErrNoKey
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

// Encrypt returns an encrypted, prefixed, base64 representation of plaintext.
// Empty input returns empty (nothing to protect). Fails if no master key.
func Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	k, err := key()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct), nil
}

// IsEncrypted reports whether s carries the encrypted marker.
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefix) }

// Decrypt reverses Encrypt. A value without the encrypted marker is returned
// unchanged (legacy plaintext). A marked value requires the master key.
func Decrypt(s string) (string, error) {
	if !IsEncrypted(s) {
		return s, nil
	}
	k, err := key()
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return "", fmt.Errorf("secretbox: decode: %w", err)
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("secretbox: ciphertext too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("secretbox: decrypt: %w", err)
	}
	return string(pt), nil
}
