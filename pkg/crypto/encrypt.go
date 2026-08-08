package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// Encrypt encrypts plaintext using AES-GCM-256 with the encryptor's key.
// Returns a versioned ciphertext string: "{version_id}:{base64(nonce+ciphertext)}"
//
// FR: FR-SEC-002 | DDS §5.5
func (e *AESEncryptor) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", fmt.Errorf("crypto: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: create GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("crypto: generate nonce: %w", err)
	}
	// Seal appends the ciphertext and GCM tag to nonce, producing nonce+ciphertext+tag.
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	encoded := base64.StdEncoding.EncodeToString(sealed)
	return e.keyVersion + ":" + encoded, nil
}
