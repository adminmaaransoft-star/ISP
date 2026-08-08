package crypto_test

import (
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

// testKey32 returns a deterministic 32-byte key for unit tests.
func testKey(n byte) []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = n
	}
	return k
}

// TestEncryptDecryptRoundTrip verifies that Encrypt followed by Decrypt
// returns the original plaintext (CRY-002, CRY-003).
func TestEncryptDecryptRoundTrip(t *testing.T) {
	store, err := crypto.NewInMemoryKeyStore(map[string][]byte{
		"v1": testKey(0x01),
	}, "v1")
	if err != nil {
		t.Fatalf("NewInMemoryKeyStore: %v", err)
	}

	enc, err := crypto.NewAESEncryptor(store)
	if err != nil {
		t.Fatalf("NewAESEncryptor: %v", err)
	}

	const plaintext = "987654321098765" // Aadhaar-like 15-digit PII

	ct, err := enc.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// Ciphertext must carry the version prefix
	if !strings.HasPrefix(ct, "v1:") {
		t.Errorf("expected ciphertext to start with 'v1:', got %q", ct)
	}
	// Must not contain plaintext
	if strings.Contains(ct, plaintext) {
		t.Errorf("plaintext must not appear in ciphertext")
	}

	got, err := crypto.Decrypt(ct, store)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if got != plaintext {
		t.Errorf("round-trip mismatch: got %q, want %q", got, plaintext)
	}
}

// TestEncryptNonDeterministic verifies that two encryptions of the same plaintext
// produce different ciphertexts (random nonce — CRY-002).
func TestEncryptNonDeterministic(t *testing.T) {
	store, _ := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": testKey(0x02)}, "v1")
	enc, _ := crypto.NewAESEncryptor(store)

	ct1, _ := enc.Encrypt("test_plaintext")
	ct2, _ := enc.Encrypt("test_plaintext")

	if ct1 == ct2 {
		t.Error("two encryptions of the same plaintext produced identical ciphertexts (nonce reuse)")
	}
}

// TestCrossRotationDecrypt verifies that ciphertext encrypted under v1 can be
// decrypted from a store that also contains v2 as active (CRY-004).
func TestCrossRotationDecrypt(t *testing.T) {
	// Encrypt with v1
	storeV1, _ := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": testKey(0xAA)}, "v1")
	encV1, _ := crypto.NewAESEncryptor(storeV1)
	ct, err := encV1.Encrypt("pan_number_ABCDE1234F")
	if err != nil {
		t.Fatalf("Encrypt v1: %v", err)
	}

	// Build store with both v1 (old) and v2 (new active)
	storeV2, err := crypto.NewInMemoryKeyStore(map[string][]byte{
		"v1": testKey(0xAA), // must keep old key for cross-rotation decryption
		"v2": testKey(0xBB),
	}, "v2")
	if err != nil {
		t.Fatalf("NewInMemoryKeyStore v2: %v", err)
	}

	// Decrypt old v1 ciphertext using the v2 store (which still has v1 key)
	got, err := crypto.Decrypt(ct, storeV2)
	if err != nil {
		t.Fatalf("cross-rotation Decrypt: %v", err)
	}
	if got != "pan_number_ABCDE1234F" {
		t.Errorf("cross-rotation: got %q, want %q", got, "pan_number_ABCDE1234F")
	}
}

// TestDecryptTamperedCiphertext verifies that GCM authentication fails on
// tampered ciphertext (integrity check — CRY-003).
func TestDecryptTamperedCiphertext(t *testing.T) {
	store, _ := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": testKey(0x03)}, "v1")
	enc, _ := crypto.NewAESEncryptor(store)

	ct, _ := enc.Encrypt("sensitive_data")

	// Tamper: flip last character of the base64 payload
	idx := strings.LastIndexByte(ct, ':')
	b64 := []byte(ct[idx+1:])
	b64[len(b64)-1] ^= 0xFF
	tampered := ct[:idx+1] + string(b64)

	_, err := crypto.Decrypt(tampered, store)
	if err == nil {
		t.Error("expected error on tampered ciphertext, got nil")
	}
}

// TestNewInMemoryKeyStore_InvalidKeyLength verifies that a non-32-byte key
// is rejected at store creation time (CRY-001).
func TestNewInMemoryKeyStore_InvalidKeyLength(t *testing.T) {
	_, err := crypto.NewInMemoryKeyStore(map[string][]byte{
		"v1": make([]byte, 16), // AES-128 key, should be rejected
	}, "v1")
	if err == nil {
		t.Error("expected error for 16-byte key, got nil")
	}
}

// TestDecryptMalformedCiphertext verifies that missing version separator returns error.
func TestDecryptMalformedCiphertext(t *testing.T) {
	store, _ := crypto.NewInMemoryKeyStore(map[string][]byte{"v1": testKey(0x04)}, "v1")
	_, err := crypto.Decrypt("notavalidciphertext", store)
	if err == nil {
		t.Error("expected error for malformed ciphertext, got nil")
	}
}

// TestDecrypt_CrossKeyVersion verifies that ciphertext sealed under any retired
// key version still decrypts after the active key has rotated forward, and that
// new writes use the current active version.
//
// INT-SEC-004 | FR-SEC-003
func TestDecrypt_CrossKeyVersion(t *testing.T) {
	// Seal one secret under each of three successive key versions.
	versions := []string{"v1", "v2", "v3"}
	keyBytes := map[string][]byte{
		"v1": testKey(0xA1),
		"v2": testKey(0xB2),
		"v3": testKey(0xC3),
	}
	plaintexts := map[string]string{
		"v1": "aadhaar_sealed_under_v1_111122223333",
		"v2": "aadhaar_sealed_under_v2_444455556666",
		"v3": "aadhaar_sealed_under_v3_777788889999",
	}

	ciphertexts := map[string]string{}
	for _, ver := range versions {
		store, err := crypto.NewInMemoryKeyStore(map[string][]byte{ver: keyBytes[ver]}, ver)
		if err != nil {
			t.Fatalf("key store %s: %v", ver, err)
		}
		enc, err := crypto.NewAESEncryptor(store)
		if err != nil {
			t.Fatalf("encryptor %s: %v", ver, err)
		}
		ct, err := enc.Encrypt(plaintexts[ver])
		if err != nil {
			t.Fatalf("encrypt under %s: %v", ver, err)
		}
		if !strings.HasPrefix(ct, ver+":") {
			t.Fatalf("ciphertext must be prefixed %s:, got %q", ver, ct)
		}
		ciphertexts[ver] = ct
	}

	// After rotation to v3 the store still holds every retired key.
	rotated, err := crypto.NewInMemoryKeyStore(keyBytes, "v3")
	if err != nil {
		t.Fatalf("rotated key store: %v", err)
	}

	for _, ver := range versions {
		got, err := crypto.Decrypt(ciphertexts[ver], rotated)
		if err != nil {
			t.Errorf("decrypt %s ciphertext after rotation to v3: %v", ver, err)
			continue
		}
		if got != plaintexts[ver] {
			t.Errorf("%s round-trip: want %q, got %q", ver, plaintexts[ver], got)
		}
	}

	// New encryptions must use the active version, not a retired one.
	enc, err := crypto.NewAESEncryptor(rotated)
	if err != nil {
		t.Fatalf("encryptor on rotated store: %v", err)
	}
	fresh, err := enc.Encrypt("newly_collected_pii")
	if err != nil {
		t.Fatalf("encrypt on rotated store: %v", err)
	}
	if !strings.HasPrefix(fresh, "v3:") {
		t.Errorf("new ciphertext must use active version v3, got %q", fresh)
	}

	// Dropping a retired key must fail loudly rather than return wrong plaintext.
	withoutV1, err := crypto.NewInMemoryKeyStore(map[string][]byte{
		"v2": keyBytes["v2"], "v3": keyBytes["v3"],
	}, "v3")
	if err != nil {
		t.Fatalf("key store without v1: %v", err)
	}
	if _, err := crypto.Decrypt(ciphertexts["v1"], withoutV1); err == nil {
		t.Error("decrypting v1 ciphertext without the v1 key must fail")
	}
}
