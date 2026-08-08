package crypto_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return path
}

func b64Key32() string {
	return base64.StdEncoding.EncodeToString(make([]byte, 32))
}

func TestLoadKeyStore_LocalScheme_Success(t *testing.T) {
	path := writeKeyFile(t, `{"active_version":"v1","keys":{"v1":"`+b64Key32()+`"}}`)

	store, err := crypto.LoadKeyStore("local:" + path)
	if err != nil {
		t.Fatalf("LoadKeyStore: %v", err)
	}
	if store.ActiveVersion() != "v1" {
		t.Errorf("ActiveVersion: want v1, got %q", store.ActiveVersion())
	}
	if _, err := store.GetKey("v1"); err != nil {
		t.Errorf("GetKey(v1): %v", err)
	}
}

func TestLoadKeyStore_EmptyURL(t *testing.T) {
	if _, err := crypto.LoadKeyStore(""); err == nil {
		t.Error("expected an error for an empty AES_KEY_STORE_URL")
	}
}

func TestLoadKeyStore_UnsupportedScheme(t *testing.T) {
	// One with a colon (exercises the scheme-extraction branch) and one
	// without (exercises the "keep the whole string as scheme" fallback).
	cases := []string{"vault://secret/keys", "nocolonatall"}
	for _, url := range cases {
		if _, err := crypto.LoadKeyStore(url); err == nil {
			t.Errorf("LoadKeyStore(%q): expected an error for an unsupported scheme", url)
		}
	}
}

func TestLoadKeyStore_LocalFileNotFound(t *testing.T) {
	if _, err := crypto.LoadKeyStore("local:" + filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Error("expected an error for a missing key file")
	}
}

func TestLoadKeyStore_LocalMalformedJSON(t *testing.T) {
	path := writeKeyFile(t, `not valid json`)
	if _, err := crypto.LoadKeyStore("local:" + path); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

func TestLoadKeyStore_LocalMissingActiveVersion(t *testing.T) {
	path := writeKeyFile(t, `{"keys":{"v1":"`+b64Key32()+`"}}`)
	if _, err := crypto.LoadKeyStore("local:" + path); err == nil {
		t.Error("expected an error when active_version is missing")
	}
}

func TestLoadKeyStore_LocalNoKeys(t *testing.T) {
	path := writeKeyFile(t, `{"active_version":"v1","keys":{}}`)
	if _, err := crypto.LoadKeyStore("local:" + path); err == nil {
		t.Error("expected an error when the key file lists no keys")
	}
}

func TestLoadKeyStore_LocalInvalidBase64Key(t *testing.T) {
	path := writeKeyFile(t, `{"active_version":"v1","keys":{"v1":"not-valid-base64!!!"}}`)
	if _, err := crypto.LoadKeyStore("local:" + path); err == nil {
		t.Error("expected an error for a non-base64 key")
	}
}

func TestLoadKeyStore_LocalWrongKeyLength(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, 16)) // AES-128, not the required 256
	path := writeKeyFile(t, `{"active_version":"v1","keys":{"v1":"`+shortKey+`"}}`)
	if _, err := crypto.LoadKeyStore("local:" + path); err == nil {
		t.Error("expected an error for a key that does not decode to 32 bytes")
	}
}

func TestLoadKeyStore_LocalActiveVersionNotInKeys(t *testing.T) {
	path := writeKeyFile(t, `{"active_version":"v2","keys":{"v1":"`+b64Key32()+`"}}`)
	if _, err := crypto.LoadKeyStore("local:" + path); err == nil {
		t.Error("expected an error when active_version is not present in keys")
	}
}
