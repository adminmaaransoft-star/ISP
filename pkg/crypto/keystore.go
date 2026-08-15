// Package crypto implements AES-GCM-256 PII encryption with key versioning.
// Ciphertext format: {version_id}:{base64(nonce+ciphertext)}  e.g. v3:Zm9vYmFy...
//
// FR: FR-SEC-002, FR-SEC-003 | DDS §5.5
package crypto

import (
	"fmt"
	"sync"
)

// KeyStore defines the interface for retrieving versioned AES-256 keys.
// Each key must be exactly 32 bytes.
type KeyStore interface {
	// GetKey returns the raw 32-byte AES key for the given version identifier.
	GetKey(versionID string) ([]byte, error)
	// ActiveVersion returns the version identifier that should be used for
	// new encryptions.
	ActiveVersion() string
}

// InMemoryKeyStore is a KeyStore backed by an in-memory map.
// Intended for testing; production deployments should use a secret-manager-backed store.
type InMemoryKeyStore struct {
	mu            sync.RWMutex
	keys          map[string][]byte
	activeVersion string
}

// NewInMemoryKeyStore constructs an InMemoryKeyStore with the supplied key map.
// activeVersion must be a key in keys.
func NewInMemoryKeyStore(keys map[string][]byte, activeVersion string) (*InMemoryKeyStore, error) {
	if _, ok := keys[activeVersion]; !ok {
		return nil, fmt.Errorf("crypto: activeVersion %q not found in key map", activeVersion)
	}
	for ver, k := range keys {
		if len(k) != 32 {
			return nil, fmt.Errorf("crypto: key for version %q must be 32 bytes, got %d", ver, len(k))
		}
	}
	cp := make(map[string][]byte, len(keys))
	for v, k := range keys {
		kb := make([]byte, 32)
		copy(kb, k)
		cp[v] = kb
	}
	return &InMemoryKeyStore{keys: cp, activeVersion: activeVersion}, nil
}

// GetKey returns the AES key for versionID.
func (s *InMemoryKeyStore) GetKey(versionID string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[versionID]
	if !ok {
		return nil, fmt.Errorf("crypto: unknown key version %q", versionID)
	}
	out := make([]byte, 32)
	copy(out, k)
	return out, nil
}

// ActiveVersion returns the current active version identifier.
func (s *InMemoryKeyStore) ActiveVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeVersion
}

// AESEncryptor holds a specific versioned key ready for encryption.
type AESEncryptor struct {
	key        []byte
	keyVersion string
}

// NewAESEncryptor creates an AESEncryptor using the active key from store.
func NewAESEncryptor(store KeyStore) (*AESEncryptor, error) {
	ver := store.ActiveVersion()
	key, err := store.GetKey(ver)
	if err != nil {
		return nil, fmt.Errorf("crypto: load active key: %w", err)
	}
	return &AESEncryptor{key: key, keyVersion: ver}, nil
}

// ActiveVersion reports the key version this encryptor writes with.
//
// Callers persist it beside the ciphertext so a later rotation can still
// decrypt: the version travels with the data rather than being inferred from
// whichever key happens to be active at read time.
func (e *AESEncryptor) ActiveVersion() string { return e.keyVersion }

// StoreDecryptor adapts a KeyStore to the single-argument Decrypt shape that
// consumers depend on, so they need no knowledge of the key store itself.
type StoreDecryptor struct{ Store KeyStore }

// Decrypt decrypts a versioned ciphertext using the wrapped store.
func (d StoreDecryptor) Decrypt(versionedCiphertext string) (string, error) {
	return Decrypt(versionedCiphertext, d.Store)
}
