package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"strings"
)

// Decrypt decrypts a versioned ciphertext produced by AESEncryptor.Encrypt.
// It resolves the key version from store, allowing cross-rotation decryption.
//
// FR: FR-SEC-002, FR-SEC-003 | DDS §5.5
func Decrypt(versionedCiphertext string, store KeyStore) (string, error) {
	parts := strings.SplitN(versionedCiphertext, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("crypto: malformed ciphertext: expected {version}:{base64}")
	}
	versionID := parts[0]

	key, err := store.GetKey(versionID)
	if err != nil {
		return "", fmt.Errorf("crypto: key lookup failed: %w", err)
	}

	raw, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("crypto: base64 decode: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("crypto: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("crypto: create GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("crypto: ciphertext too short")
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("crypto: GCM open failed (wrong key or tampered data): %w", err)
	}
	return string(plaintext), nil
}
