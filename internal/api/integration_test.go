//go:build integration

// Integration tests for the subscriber API.
//
// Covers INT-SEC-003 from the Integration Tests tracker sheet: PII submitted to
// POST /api/v1/subscribers must reach storage encrypted and version-prefixed,
// never in plaintext.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/api -Tags integration
package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/maaransoft/isp-bss-oss/pkg/crypto"
)

const itJWTSecret = "integration_jwt_secret_32_chars!!"

// ── Recording stores ────────────────────────────────────────────────────────

// itKYCStore records what would be written to kyc_verifications.
type itKYCStore struct {
	mu   sync.Mutex
	rows []itKYCRow
}

type itKYCRow struct {
	SubscriberID int
	AadhaarEnc   string
	PANEnc       string
	KeyVersion   string
}

func (s *itKYCStore) UpsertKYC(_ context.Context, subscriberID int, aadhaarEnc, panEnc, keyVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, itKYCRow{subscriberID, aadhaarEnc, panEnc, keyVersion})
	return nil
}

func (s *itKYCStore) snapshot() []itKYCRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]itKYCRow(nil), s.rows...)
}

// itSubscriberStore records subscriber inserts, including the password hash.
type itSubscriberStore struct {
	mu     sync.Mutex
	rows   []api.SubscriberRecord
	hashes []string
	nextID int
}

func (s *itSubscriberStore) CreateSubscriber(_ context.Context, sub api.SubscriberRecord, passwordHash string) (*api.SubscriberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	sub.ID = s.nextID
	sub.CreatedAt = time.Now()
	s.rows = append(s.rows, sub)
	s.hashes = append(s.hashes, passwordHash)
	return &sub, nil
}

func (s *itSubscriberStore) GetSubscriberByID(_ context.Context, id int) (*api.SubscriberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].ID == id {
			return &s.rows[i], nil
		}
	}
	return nil, nil
}

func (s *itSubscriberStore) UpdateSubscriber(ctx context.Context, id int, _ *int, _ *string) (*api.SubscriberRecord, error) {
	return s.GetSubscriberByID(ctx, id)
}

func (s *itSubscriberStore) GetSubscriberByUsername(_ context.Context, username string) (*api.SubscriberRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rows {
		if s.rows[i].Username == username {
			return &s.rows[i], nil
		}
	}
	return nil, nil
}

// dump returns everything the store holds, as the raw text a `SELECT *` would
// show — used to assert plaintext PII appears nowhere in persisted state.
func (s *itSubscriberStore) dump() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.rows)
	return string(b) + strings.Join(s.hashes, " ")
}

func itAdminToken(t *testing.T) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             "billing_admin",
		RegisteredClaims: jwt.RegisteredClaims{Subject: "admin", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign admin token: %v", err)
	}
	return tok
}

func itKeyStore(t *testing.T, active string, versions ...string) crypto.KeyStore {
	t.Helper()
	keys := map[string][]byte{}
	for i, v := range versions {
		k := make([]byte, 32)
		for j := range k {
			k[j] = byte(i + 1)
		}
		keys[v] = k
	}
	ks, err := crypto.NewInMemoryKeyStore(keys, active)
	if err != nil {
		t.Fatalf("key store: %v", err)
	}
	return ks
}

// ── INT-SEC-003 ─────────────────────────────────────────────────────────────

// TestCreateSubscriber_EncryptsPII verifies Aadhaar and PAN submitted through
// the API are stored as version-prefixed ciphertext, decrypt back to the
// original values, and never appear in plaintext in persisted state.
//
// INT-SEC-003 | FR-SEC-002
func TestCreateSubscriber_EncryptsPII(t *testing.T) {
	const (
		aadhaar = "123456789012"
		pan     = "ABCDE1234F"
	)

	subs := &itSubscriberStore{}
	kyc := &itKYCStore{}
	keyStore := itKeyStore(t, "v1", "v1")

	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: kyc, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: keyStore,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber:       "CAF-2026-0001",
		Username:        "newsub@isp",
		Password:        "initial-password",
		MobileNumber:    "+919876543210",
		Email:           "sub@example.com",
		PlanID:          1,
		RegisteredState: "TN",
		Aadhaar:         aadhaar,
		PAN:             pan,
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	rows := kyc.snapshot()
	if len(rows) != 1 {
		t.Fatalf("want 1 kyc_verifications row, got %d", len(rows))
	}
	row := rows[0]

	// Ciphertext must carry the key version prefix so a later rotation can still
	// resolve which key it was sealed under.
	for label, ct := range map[string]string{"aadhaar_encrypted": row.AadhaarEnc, "pan_encrypted": row.PANEnc} {
		if ct == "" {
			t.Errorf("%s is empty", label)
			continue
		}
		if !strings.HasPrefix(ct, "v") || !strings.Contains(ct, ":") {
			t.Errorf("%s must be {version}:{base64}, got %q", label, ct)
		}
	}
	if row.KeyVersion != "v1" {
		t.Errorf("key_version: want v1, got %q", row.KeyVersion)
	}

	// Plaintext must not survive anywhere in what was persisted.
	haystack := row.AadhaarEnc + " " + row.PANEnc + " " + subs.dump() + " " + rec.Body.String()
	for label, secret := range map[string]string{"aadhaar": aadhaar, "PAN": pan} {
		if strings.Contains(haystack, secret) {
			t.Errorf("plaintext %s found in persisted state or API response", label)
		}
	}

	// And the ciphertext must decrypt back to the submitted values.
	gotAadhaar, err := crypto.Decrypt(row.AadhaarEnc, keyStore)
	if err != nil {
		t.Fatalf("decrypt aadhaar: %v", err)
	}
	if gotAadhaar != aadhaar {
		t.Errorf("decrypted aadhaar: want %q, got %q", aadhaar, gotAadhaar)
	}
	gotPAN, err := crypto.Decrypt(row.PANEnc, keyStore)
	if err != nil {
		t.Fatalf("decrypt PAN: %v", err)
	}
	if gotPAN != pan {
		t.Errorf("decrypted PAN: want %q, got %q", pan, gotPAN)
	}
}

// TestCreateSubscriber_PasswordNeverStoredInClear verifies the submitted
// password is bcrypt-hashed before it reaches the store.
//
// INT-SEC-003 (supporting) | FR-SEC-002
func TestCreateSubscriber_PasswordNeverStoredInClear(t *testing.T) {
	const password = "sup3r-s3cret-pw"

	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber:       "CAF-2026-0002",
		Username:        "pwtest@isp",
		Password:        password,
		MobileNumber:    "+919876543211",
		PlanID:          1,
		RegisteredState: "TN",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(subs.dump(), password) {
		t.Error("plaintext password reached the subscriber store")
	}
	if len(subs.hashes) != 1 || !strings.HasPrefix(subs.hashes[0], "$2") {
		t.Errorf("want a bcrypt hash, got %v", subs.hashes)
	}
}

// TestCreateSubscriber_RequiresAdminRole verifies subscriber creation is closed
// to non-admin roles.
//
// INT-SEC-003 (supporting) | FR-SEC-005
func TestCreateSubscriber_RequiresAdminRole(t *testing.T) {
	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	csrToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             "csr",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign csr token: %v", err)
	}

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber: "CAF-X", Username: "x@isp", MobileNumber: "+91987", PlanID: 1, RegisteredState: "TN",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+csrToken)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("want 403 for csr, got %d", rec.Code)
	}
	if len(subs.rows) != 0 {
		t.Error("no subscriber may be created by a forbidden role")
	}
}

// TestCreateSubscriber_RejectsNonE164Phone verifies the DoD Phase 2 Step 4
// fix: mobile_number must be valid E.164, checked before anything reaches
// the store.
//
// DoD Phase 2 Step 4 | FR-SUB (subscriber onboarding)
func TestCreateSubscriber_RejectsNonE164Phone(t *testing.T) {
	cases := []struct {
		name  string
		phone string
	}{
		{"missing leading +", "919876543210"},
		{"contains a space", "+91 9876543210"},
		{"contains a dash", "+91-9876543210"},
		{"leading zero after +", "+0919876543210"},
		{"not a phone number at all", "not-a-phone"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subs := &itSubscriberStore{}
			h := api.NewHandler(api.HandlerDeps{
				DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
			})
			mux := http.NewServeMux()
			h.RegisterRoutes(mux, itJWTSecret)

			body, _ := json.Marshal(api.CreateSubscriberRequest{
				CAFNumber: "CAF-BAD", Username: "bad@isp", Password: "pw",
				MobileNumber: tc.phone, PlanID: 1, RegisteredState: "TN",
			})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("want 422, got %d — %s", rec.Code, rec.Body.String())
			}
			if len(subs.rows) != 0 {
				t.Error("no subscriber may be created with an invalid phone number")
			}
		})
	}
}

// TestCreateSubscriber_AcceptsValidE164Phone is the positive counterpart to
// TestCreateSubscriber_RejectsNonE164Phone.
func TestCreateSubscriber_AcceptsValidE164Phone(t *testing.T) {
	subs := &itSubscriberStore{}
	h := api.NewHandler(api.HandlerDeps{
		DB: subs, KYC: &itKYCStore{}, Wallet: billing.NewWalletService(&stubWallet{}), KeyStore: itKeyStore(t, "v1", "v1"),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body, _ := json.Marshal(api.CreateSubscriberRequest{
		CAFNumber: "CAF-GOOD", Username: "good@isp", Password: "pw",
		MobileNumber: "+919876543210", PlanID: 1, RegisteredState: "TN",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itAdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(subs.rows) != 1 {
		t.Fatalf("want 1 subscriber created, got %d", len(subs.rows))
	}
}
