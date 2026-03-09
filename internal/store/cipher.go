package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
)

// Cipher handles AES-256-GCM encryption/decryption for secrets at rest.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher creates a Cipher from a 64-character hex-encoded key (32 bytes).
func NewCipher(hexKey string) (*Cipher, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("decoding encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("creating AES cipher: %w", err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}

	return &Cipher{aead: aead}, nil
}

// Encrypt encrypts plaintext and returns nonce+ciphertext as a single byte slice.
func (c *Cipher) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt decrypts a nonce+ciphertext byte slice and returns the plaintext.
func (c *Cipher) Decrypt(ciphertext []byte) (string, error) {
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypting: %w", err)
	}
	return string(plaintext), nil
}

// EncryptOptional encrypts a string, returning nil if the input is empty.
func (c *Cipher) EncryptOptional(plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	return c.Encrypt(plaintext)
}

// DecryptOptional decrypts a byte slice, returning "" if the input is nil.
func (c *Cipher) DecryptOptional(ciphertext []byte) (string, error) {
	if ciphertext == nil {
		return "", nil
	}
	return c.Decrypt(ciphertext)
}
