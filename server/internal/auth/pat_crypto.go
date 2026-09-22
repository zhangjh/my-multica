package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// PAT ciphertext format: base64( nonce || aes-gcm-seal(plaintext) ). The
// 12-byte random nonce is prepended to the AES-GCM output (ciphertext + 16-byte
// tag) and the whole thing is base64-encoded for storage in a TEXT column.
//
// The encryption key is derived from the deployment's JWT_SECRET via SHA-256
// so no new secret needs to be configured. Consequence: ciphertexts written
// under one JWT_SECRET cannot be decrypted after the secret is rotated — the
// reveal endpoint reports failure for those rows. That is acceptable: tokens
// minted under an old secret were stored with an old key, and the operator who
// rotated the secret is the one who can mint replacements.

func patCipherKey() []byte {
	sum := sha256.Sum256(JWTSecret())
	return sum[:]
}

// EncryptPATToken seals a raw token string and returns its base64 encoding.
func EncryptPATToken(raw string) (string, error) {
	block, err := aes.NewCipher(patCipherKey())
	if err != nil {
		return "", fmt.Errorf("encrypt PAT token: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("encrypt PAT token: %w", err)
	}

	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("encrypt PAT token: %w", err)
	}

	sealed := aead.Seal(nonce, nonce, []byte(raw), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// ErrPATCannotReveal is returned when a PAT row carries no encrypted copy
// (tokens minted before this feature existed) or the ciphertext cannot be
// decrypted with the current key.
var ErrPATCannotReveal = errors.New("token cannot be revealed")

// DecryptPATToken unseals a token previously produced by EncryptPATToken.
func DecryptPATToken(encoded string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("%w: malformed ciphertext", ErrPATCannotReveal)
	}

	block, err := aes.NewCipher(patCipherKey())
	if err != nil {
		return "", fmt.Errorf("decrypt PAT token: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("decrypt PAT token: %w", err)
	}
	if len(sealed) < aead.NonceSize() {
		return "", fmt.Errorf("%w: truncated ciphertext", ErrPATCannotReveal)
	}

	raw, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("%w: decryption failed (JWT_SECRET rotated?)", ErrPATCannotReveal)
	}
	return string(raw), nil
}