package auth

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestPATCryptoRoundtrip(t *testing.T) {
	token := "mul_" + strings.Repeat("ab", 20)
	cipher, err := EncryptPATToken(token)
	if err != nil {
		t.Fatalf("EncryptPATToken: %v", err)
	}
	if cipher == "" || cipher == token {
		t.Fatalf("ciphertext looks wrong: %q", cipher)
	}

	raw, err := DecryptPATToken(cipher)
	if err != nil {
		t.Fatalf("DecryptPATToken: %v", err)
	}
	if raw != token {
		t.Fatalf("roundtrip mismatch: got %q, want %q", raw, token)
	}
}

func TestPATCryptoIsRandomized(t *testing.T) {
	token := "mul_0123456789abcdef"
	c1, err := EncryptPATToken(token)
	if err != nil {
		t.Fatalf("EncryptPATToken: %v", err)
	}
	c2, err := EncryptPATToken(token)
	if err != nil {
		t.Fatalf("EncryptPATToken: %v", err)
	}
	if c1 == c2 {
		t.Fatalf("two encryptions of the same token produced identical ciphertext — nonce is not randomized")
	}
}

func TestPATCryptoRejectsMalformed(t *testing.T) {
	if _, err := DecryptPATToken(""); err == nil {
		t.Fatal("expected error for empty ciphertext")
	}
	if _, err := DecryptPATToken("!!not-base64!!"); err == nil {
		t.Fatal("expected error for non-base64 ciphertext")
	}

	// Truncate a real ciphertext so the auth tag fails / data is short.
	cipher, err := EncryptPATToken("mul_tamper-me")
	if err != nil {
		t.Fatalf("EncryptPATToken: %v", err)
	}
	sealed, _ := base64.StdEncoding.DecodeString(cipher)
	tampered := base64.StdEncoding.EncodeToString(sealed[:len(sealed)-2])
	if _, err := DecryptPATToken(tampered); err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}