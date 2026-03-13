package store

import (
	"encoding/hex"
	"testing"
)

func testKey() string {
	// 32 random bytes hex-encoded = 64 chars
	return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}

func TestCipher_EncryptDecrypt(t *testing.T) {
	c, err := NewCipher(testKey())
	if err != nil {
		t.Fatalf("failed to create cipher: %v", err)
	}

	plaintext := "xoxb-secret-bot-token"
	encrypted, err := c.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt failed: %v", err)
	}

	if string(encrypted) == plaintext {
		t.Error("encrypted should differ from plaintext")
	}

	decrypted, err := c.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt failed: %v", err)
	}

	if decrypted != plaintext {
		t.Errorf("roundtrip failed: got %q, want %q", decrypted, plaintext)
	}
}

func TestCipher_DifferentCiphertexts(t *testing.T) {
	c, _ := NewCipher(testKey())
	enc1, _ := c.Encrypt("same")
	enc2, _ := c.Encrypt("same")
	if hex.EncodeToString(enc1) == hex.EncodeToString(enc2) {
		t.Error("same plaintext should produce different ciphertexts (random nonce)")
	}
}

func TestCipher_WrongKey(t *testing.T) {
	c1, _ := NewCipher(testKey())
	encrypted, _ := c1.Encrypt("secret")

	c2, _ := NewCipher("abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789")
	_, err := c2.Decrypt(encrypted)
	if err == nil {
		t.Error("expected error when decrypting with wrong key")
	}
}

func TestCipher_EncryptOptional_Empty(t *testing.T) {
	c, _ := NewCipher(testKey())
	result, err := c.EncryptOptional("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Error("expected nil for empty plaintext")
	}
}

func TestCipher_DecryptOptional_Nil(t *testing.T) {
	c, _ := NewCipher(testKey())
	result, err := c.DecryptOptional(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "" {
		t.Error("expected empty string for nil ciphertext")
	}
}

func TestCipher_InvalidKeyLength(t *testing.T) {
	_, err := NewCipher("tooshort")
	if err == nil {
		t.Error("expected error for short key")
	}
}

func TestCipher_InvalidKeyHex(t *testing.T) {
	_, err := NewCipher("zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
	if err == nil {
		t.Error("expected error for invalid hex")
	}
}

func TestCipher_DecryptTooShort(t *testing.T) {
	c, _ := NewCipher(testKey())
	_, err := c.Decrypt([]byte{1, 2, 3})
	if err == nil {
		t.Error("expected error for short ciphertext")
	}
}
