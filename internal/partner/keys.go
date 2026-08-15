// Package partner implements the third-party integration surface: scoped API
// keys distinct from staff JWTs, and signed outbound webhooks.
//
// FR: FR-API-001..003 | MDS §4.22 | DBD §6.8
package partner

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Key format: pk_live_<8 hex prefix>_<48 hex secret>.
//
// The prefix is a lookup handle, not a secret. Keys are stored hashed, so the
// server cannot search by the key itself; it parses the prefix, fetches that
// one row and compares — one hash per request rather than one per stored key.
// It is also what the console displays, since the key is shown exactly once.
const (
	KeyEnvLive = "pk_live"
	KeyEnvTest = "pk_test"

	prefixBytes = 4  // 8 hex chars
	secretBytes = 24 // 48 hex chars — 192 bits
)

// Scopes a key can carry. Read and write are separate so a partner that only
// ingests data cannot mutate it by holding one credential.
const (
	ScopeReadSubscribers  = "read:subscribers"
	ScopeWriteSubscribers = "write:subscribers"
	ScopeReadInvoices     = "read:invoices"
	ScopeReadTickets      = "read:tickets"
	ScopeWriteTickets     = "write:tickets"
	ScopeManageWebhooks   = "manage:webhooks"
)

// ValidScopes is the closed set. A key requesting anything outside it is
// refused at creation rather than silently granted a scope no route checks —
// which would read as working until the day someone relied on it.
var ValidScopes = map[string]bool{
	ScopeReadSubscribers:  true,
	ScopeWriteSubscribers: true,
	ScopeReadInvoices:     true,
	ScopeReadTickets:      true,
	ScopeWriteTickets:     true,
	ScopeManageWebhooks:   true,
}

// APIKey is a partner credential as stored. The plaintext key is never held
// here: it exists only in the response to the creating request.
type APIKey struct {
	ID          int        `json:"id"`
	PartnerName string     `json:"partner_name"`
	KeyPrefix   string     `json:"key_prefix"`
	Scopes      []string   `json:"scopes"`
	Active      bool       `json:"active"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// GeneratedKey is returned once, at creation. Plaintext is not recoverable
// afterwards by design — a key an operator can re-read from the console is one
// an attacker with console access can too.
type GeneratedKey struct {
	Plaintext string
	Prefix    string
	Hash      string
}

// GenerateKey mints a new API key.
func GenerateKey(env string) (*GeneratedKey, error) {
	if env != KeyEnvLive && env != KeyEnvTest {
		return nil, fmt.Errorf("partner: unknown key environment %q", env)
	}

	prefixRaw := make([]byte, prefixBytes)
	if _, err := rand.Read(prefixRaw); err != nil {
		return nil, fmt.Errorf("partner: generate key prefix: %w", err)
	}
	secretRaw := make([]byte, secretBytes)
	if _, err := rand.Read(secretRaw); err != nil {
		return nil, fmt.Errorf("partner: generate key secret: %w", err)
	}

	prefix := env + "_" + hex.EncodeToString(prefixRaw)
	plaintext := prefix + "_" + hex.EncodeToString(secretRaw)

	return &GeneratedKey{
		Plaintext: plaintext,
		Prefix:    prefix,
		Hash:      HashKey(plaintext),
	}, nil
}

// HashKey returns the stored representation of a key.
//
// SHA-256, deliberately, where subscriber passwords use bcrypt. The two cases
// are not alike: a password is human-chosen and low-entropy, so the work
// factor is what stops a dictionary attack on a stolen hash. An API key is 192
// bits of CSPRNG output — there is no dictionary, and no feasible search — so
// bcrypt would add ~100ms to every partner request and buy nothing. Salting is
// pointless for the same reason: there are no duplicate keys to correlate.
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// ParsePrefix extracts the lookup prefix from a presented key.
//
// A key is pk_{env}_{prefix}_{secret}, so it splits into exactly four parts.
// Anything not shaped like that is rejected here, costing a string split
// rather than a database round trip — which also means a scanner spraying
// random Authorization headers never reaches Postgres.
func ParsePrefix(presented string) (string, bool) {
	parts := strings.Split(presented, "_")
	if len(parts) != 4 {
		return "", false
	}
	if parts[0] != "pk" {
		return "", false
	}
	if parts[1] != "live" && parts[1] != "test" {
		return "", false
	}
	if len(parts[2]) != prefixBytes*2 || len(parts[3]) != secretBytes*2 {
		return "", false
	}
	return parts[0] + "_" + parts[1] + "_" + parts[2], true
}

// VerifyKey reports whether a presented key matches a stored hash.
//
// Constant-time despite the hash being public-ish: the comparison runs on
// every partner request, and a timing side channel on a credential check is
// not worth reasoning about case by case.
func VerifyKey(presented, storedHash string) bool {
	computed := HashKey(presented)
	return subtle.ConstantTimeCompare([]byte(computed), []byte(storedHash)) == 1
}

// ValidateScopes rejects unknown or empty scope sets.
func ValidateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("partner: a key needs at least one scope")
	}
	for _, s := range scopes {
		if !ValidScopes[s] {
			return fmt.Errorf("partner: unknown scope %q", s)
		}
	}
	return nil
}

// HasScope reports whether a key carries a scope.
func HasScope(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

// Usable reports whether a key may authenticate right now.
//
// Revocation, deactivation and expiry are checked here rather than in the SQL
// so all three reasons a key stops working live in one place — a key that is
// active but expired must fail exactly like a revoked one.
func (k *APIKey) Usable(now time.Time) bool {
	if k == nil || !k.Active || k.RevokedAt != nil {
		return false
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		return false
	}
	return true
}
