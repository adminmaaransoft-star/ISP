package crypto

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// keyFileFormat is the on-disk layout of a local key store.
//
//	{
//	  "active_version": "v2",
//	  "keys": { "v1": "<base64 32 bytes>", "v2": "<base64 32 bytes>" }
//	}
//
// Retired versions must stay listed: ciphertext is version-prefixed, and
// dropping an old key makes every value sealed under it permanently
// undecryptable (FR-SEC-003).
type keyFileFormat struct {
	ActiveVersion string            `json:"active_version"`
	Keys          map[string]string `json:"keys"`
}

// LoadKeyStore builds a KeyStore from an AES_KEY_STORE_URL.
//
// Only the "local:" scheme is implemented. Production deployments are expected
// to point at a secret manager, and an unrecognised scheme fails loudly rather
// than silently falling back to a file — a silent fallback would be a way to
// run production on a key checked into the repo.
func LoadKeyStore(url string) (KeyStore, error) {
	switch {
	case strings.HasPrefix(url, "local:"):
		return loadLocalKeyStore(strings.TrimPrefix(url, "local:"))
	case url == "":
		return nil, fmt.Errorf("crypto: AES_KEY_STORE_URL is empty")
	default:
		scheme := url
		if i := strings.Index(url, ":"); i > 0 {
			scheme = url[:i]
		}
		return nil, fmt.Errorf("crypto: unsupported key store scheme %q (only \"local:\" is implemented)", scheme)
	}
}

func loadLocalKeyStore(path string) (KeyStore, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path comes from operator configuration
	if err != nil {
		return nil, fmt.Errorf("crypto: read key file %s: %w", path, err)
	}

	var file keyFileFormat
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("crypto: parse key file %s: %w", path, err)
	}
	if file.ActiveVersion == "" {
		return nil, fmt.Errorf("crypto: key file %s has no active_version", path)
	}
	if len(file.Keys) == 0 {
		return nil, fmt.Errorf("crypto: key file %s lists no keys", path)
	}

	keys := make(map[string][]byte, len(file.Keys))
	for version, encoded := range file.Keys {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("crypto: key %q in %s is not valid base64: %w", version, path, err)
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("crypto: key %q in %s must decode to 32 bytes for AES-256, got %d",
				version, path, len(key))
		}
		keys[version] = key
	}

	store, err := NewInMemoryKeyStore(keys, file.ActiveVersion)
	if err != nil {
		return nil, fmt.Errorf("crypto: build key store from %s: %w", path, err)
	}
	return store, nil
}
