//go:build integration

// NAS management API — FR-NAS-001..004, FR-HSP-002 | MDS §4.11, §4.23.
//
// Route wiring, authorisation and — mostly — the two secret-handling
// guarantees: a plaintext RADIUS secret is never stored, and no secret in any
// form is ever returned.
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

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// ── Stubs ───────────────────────────────────────────────────────────────────

type stubNAS struct {
	mu sync.Mutex

	created   []nas.NewNASDevice
	updates   []nas.NASDeviceUpdate
	listed    []nas.DeviceSummary
	missing   bool
	createErr error
}

func (s *stubNAS) ListNASDeviceSummaries(context.Context) ([]nas.DeviceSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listed, nil
}

func (s *stubNAS) CreateNASDevice(_ context.Context, d nas.NewNASDevice) (*nas.DeviceSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.created = append(s.created, d)
	return &nas.DeviceSummary{
		ID: len(s.created), IP: d.IP, Vendor: d.Vendor,
		Description: d.Description, CoAPort: d.CoAPort, PoDPort: d.PoDPort, AllowMAB: d.AllowMAB,
	}, nil
}

func (s *stubNAS) UpdateNASDevice(_ context.Context, id int, u nas.NASDeviceUpdate) (*nas.DeviceSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.missing {
		return nil, nil
	}
	s.updates = append(s.updates, u)
	out := &nas.DeviceSummary{ID: id, IP: "203.0.113.9", Vendor: "mikrotik"}
	if u.AllowMAB != nil {
		out.AllowMAB = *u.AllowMAB
	}
	return out, nil
}

func (s *stubNAS) snapshot() ([]nas.NewNASDevice, []nas.NASDeviceUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]nas.NewNASDevice(nil), s.created...), append([]nas.NASDeviceUpdate(nil), s.updates...)
}

// stubEncryptor records what it was asked to encrypt, so a test can prove the
// plaintext never went past it.
type stubEncryptor struct {
	mu        sync.Mutex
	plaintext []string
}

func (e *stubEncryptor) Encrypt(plaintext string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.plaintext = append(e.plaintext, plaintext)
	return "v1:encrypted(" + plaintext + ")", nil
}

func (e *stubEncryptor) ActiveVersion() string { return "v1" }

// ── Harness ─────────────────────────────────────────────────────────────────

func nasMux(store api.NASQuerier, enc api.SecretEncryptor) *http.ServeMux {
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}, NAS: store, SecretEncryptor: enc})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)
	return mux
}

func nasCall(t *testing.T, mux *http.ServeMux, method, path, role, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)) //nolint:noctx // httptest.NewRequestWithContext needs go1.23; module is go1.22
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+hotspotStaffToken(t, role))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ── Registration ────────────────────────────────────────────────────────────

// TestFR_NAS_001_RegisteringEncryptsTheSecretAndNeverEchoesIt is the pair of
// guarantees this surface exists to keep.
func TestFR_NAS_001_RegisteringEncryptsTheSecretAndNeverEchoesIt(t *testing.T) {
	store := &stubNAS{}
	enc := &stubEncryptor{}
	const secret = "a-very-long-shared-secret"

	rec := nasCall(t, nasMux(store, enc), http.MethodPost, "/api/v1/nas", "noc_engineer",
		`{"ip":"203.0.113.10","vendor":"mikrotik","radius_secret":"`+secret+`","allow_mab":true}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}

	created, _ := store.snapshot()
	if len(created) != 1 {
		t.Fatalf("want 1 created device, got %d", len(created))
	}
	if created[0].SecretEncrypted == secret {
		t.Fatal("the plaintext RADIUS secret reached storage — it must be encrypted first")
	}
	if !strings.HasPrefix(created[0].SecretEncrypted, "v1:") || created[0].KeyVersion != "v1" {
		t.Errorf("secret must be stored encrypted with its key version, got %q / %q",
			created[0].SecretEncrypted, created[0].KeyVersion)
	}
	if created[0].AllowMAB != true {
		t.Error("allow_mab must be carried through — it is the hotspot prerequisite")
	}
	// Defaults applied for the MikroTik control port.
	if created[0].CoAPort != 1700 || created[0].PoDPort != 1700 {
		t.Errorf("control ports should default to 1700, got %d/%d", created[0].CoAPort, created[0].PoDPort)
	}

	// Nothing secret comes back out.
	if bytes.Contains(rec.Body.Bytes(), []byte(secret)) ||
		bytes.Contains(rec.Body.Bytes(), []byte(created[0].SecretEncrypted)) {
		t.Errorf("the response must contain neither the secret nor its ciphertext: %s", rec.Body.String())
	}
}

func TestFR_NAS_001_ListingNeverCarriesASecret(t *testing.T) {
	store := &stubNAS{listed: []nas.DeviceSummary{
		{ID: 1, IP: "203.0.113.10", Vendor: "mikrotik", AllowMAB: true},
	}}

	rec := nasCall(t, nasMux(store, &stubEncryptor{}), http.MethodGet, "/api/v1/nas", "noc_engineer", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	for _, forbidden := range []string{"secret", "radius_secret", "key_version"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), forbidden) {
			t.Errorf("a NAS listing must not mention %q: %s", forbidden, rec.Body.String())
		}
	}

	// The supported vendor list is surfaced so an operator does not discover the
	// accepted values via a 422.
	var resp struct {
		SupportedVendors []string `json:"supported_vendors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.SupportedVendors) < 5 {
		t.Errorf("want the vendor list, got %v", resp.SupportedVendors)
	}
}

func TestFR_NAS_001_RegistrationValidation(t *testing.T) {
	tests := []struct {
		name, body string
		want       int
	}{
		{"bad ip", `{"ip":"not-an-ip","vendor":"mikrotik","radius_secret":"0123456789abcdef"}`, http.StatusUnprocessableEntity},
		{"unknown vendor", `{"ip":"203.0.113.1","vendor":"acme","radius_secret":"0123456789abcdef"}`, http.StatusUnprocessableEntity},
		{"no secret", `{"ip":"203.0.113.1","vendor":"mikrotik"}`, http.StatusUnprocessableEntity},
		{"short secret", `{"ip":"203.0.113.1","vendor":"mikrotik","radius_secret":"short"}`, http.StatusUnprocessableEntity},
		{"bad port", `{"ip":"203.0.113.1","vendor":"mikrotik","radius_secret":"0123456789abcdef","coa_port":70000}`, http.StatusUnprocessableEntity},
		{"malformed", `{`, http.StatusBadRequest},
		{"valid", `{"ip":"203.0.113.1","vendor":"mikrotik","radius_secret":"0123456789abcdef"}`, http.StatusCreated},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &stubNAS{}
			rec := nasCall(t, nasMux(store, &stubEncryptor{}), http.MethodPost, "/api/v1/nas", "noc_engineer", tc.body)
			if rec.Code != tc.want {
				t.Errorf("want %d, got %d — %s", tc.want, rec.Code, rec.Body.String())
			}
			created, _ := store.snapshot()
			if tc.want != http.StatusCreated && len(created) != 0 {
				t.Error("a rejected registration must not reach the store")
			}
		})
	}
}

// TestFR_NAS_001_NoEncryptorMeansNoRegistration — storing a plaintext secret
// would be indistinguishable from a correct row until someone read the table.
func TestFR_NAS_001_NoEncryptorMeansNoRegistration(t *testing.T) {
	store := &stubNAS{}
	rec := nasCall(t, nasMux(store, nil), http.MethodPost, "/api/v1/nas", "noc_engineer",
		`{"ip":"203.0.113.1","vendor":"mikrotik","radius_secret":"0123456789abcdef"}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("want 503 with no key store configured, got %d", rec.Code)
	}
	if created, _ := store.snapshot(); len(created) != 0 {
		t.Error("nothing may be stored without an encryptor")
	}
}

// ── The allow_mab toggle ────────────────────────────────────────────────────

func TestFR_HSP_002_AllowMABTogglesWithoutResubmittingTheSecret(t *testing.T) {
	store := &stubNAS{}
	enc := &stubEncryptor{}

	rec := nasCall(t, nasMux(store, enc), http.MethodPatch, "/api/v1/nas/4", "noc_engineer",
		`{"allow_mab":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	_, updates := store.snapshot()
	if len(updates) != 1 {
		t.Fatalf("want 1 update, got %d", len(updates))
	}
	if updates[0].AllowMAB == nil || !*updates[0].AllowMAB {
		t.Error("allow_mab must reach the store")
	}
	// Every other field stays nil, so the store leaves them alone.
	if updates[0].SecretEncrypted != nil || updates[0].Vendor != nil || updates[0].CoAPort != nil {
		t.Errorf("an allow_mab-only patch must not touch anything else: %+v", updates[0])
	}
	if len(enc.plaintext) != 0 {
		t.Error("no secret was submitted, so nothing should have been encrypted")
	}

	// The response tells the operator the change is not instant — the resolver
	// caches for 60s, and without this they test immediately and conclude the
	// toggle failed.
	if !strings.Contains(rec.Body.String(), "60s") {
		t.Errorf("the response should mention the cache refresh window: %s", rec.Body.String())
	}
}

func TestFR_NAS_001_SecretRotationCarriesItsKeyVersion(t *testing.T) {
	store := &stubNAS{}
	enc := &stubEncryptor{}

	rec := nasCall(t, nasMux(store, enc), http.MethodPatch, "/api/v1/nas/4", "noc_engineer",
		`{"radius_secret":"a-brand-new-shared-secret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}

	_, updates := store.snapshot()
	if updates[0].SecretEncrypted == nil || updates[0].KeyVersion == nil {
		t.Fatal("a rotated secret must carry its key version, or it becomes undecryptable")
	}
	if *updates[0].SecretEncrypted == "a-brand-new-shared-secret" {
		t.Error("the plaintext reached the store")
	}

	// An explicit empty string is refused rather than silently blanking the
	// secret, which would take the NAS offline at the next cache refresh.
	blank := nasCall(t, nasMux(&stubNAS{}, enc), http.MethodPatch, "/api/v1/nas/4", "noc_engineer",
		`{"radius_secret":""}`)
	if blank.Code != http.StatusUnprocessableEntity {
		t.Errorf("an empty secret must be refused, got %d", blank.Code)
	}
}

func TestFR_NAS_001_UpdatingAMissingDeviceIs404(t *testing.T) {
	rec := nasCall(t, nasMux(&stubNAS{missing: true}, &stubEncryptor{}),
		http.MethodPatch, "/api/v1/nas/999", "noc_engineer", `{"allow_mab":true}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d", rec.Code)
	}
}

// ── Authorisation ───────────────────────────────────────────────────────────

// TestFR_NAS_001_NASRoutesAreNOCOnly — which NAS exist, on which addresses,
// with MAB on or off, is a map of the network's soft spots rather than routine
// support data.
func TestFR_NAS_001_NASRoutesAreNOCOnly(t *testing.T) {
	allowed := []string{"noc_engineer", "isp_owner"}
	refused := []string{"csr", "technician", "billing_admin", "subscriber"}

	for _, role := range allowed {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodGet, "/api/v1/nas", ""},
			{http.MethodPost, "/api/v1/nas", `{"ip":"203.0.113.1","vendor":"mikrotik","radius_secret":"0123456789abcdef"}`},
			{http.MethodPatch, "/api/v1/nas/1", `{"allow_mab":true}`},
		} {
			rec := nasCall(t, nasMux(&stubNAS{}, &stubEncryptor{}), tc.method, tc.path, role, tc.body)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s must reach %s %s, got %d", role, tc.method, tc.path, rec.Code)
			}
		}
	}

	for _, role := range refused {
		rec := nasCall(t, nasMux(&stubNAS{}, &stubEncryptor{}), http.MethodGet, "/api/v1/nas", role, "")
		if rec.Code != http.StatusForbidden && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s must not read the NAS inventory, got %d", role, rec.Code)
		}
		patch := nasCall(t, nasMux(&stubNAS{}, &stubEncryptor{}), http.MethodPatch, "/api/v1/nas/1", role, `{"allow_mab":true}`)
		if patch.Code != http.StatusForbidden && patch.Code != http.StatusUnauthorized {
			t.Errorf("%s must not toggle allow_mab, got %d", role, patch.Code)
		}
	}

	// And no anonymous reach at all.
	anon := nasCall(t, nasMux(&stubNAS{}, &stubEncryptor{}), http.MethodGet, "/api/v1/nas", "", "")
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("want 401 without a token, got %d", anon.Code)
	}
}

func TestFR_NAS_001_DegradesTo503WhenUnconfigured(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{DB: &stubDB{}, KYC: &stubKYC{}})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/nas", ""},
		{http.MethodPost, "/api/v1/nas", `{"ip":"203.0.113.1","vendor":"mikrotik","radius_secret":"0123456789abcdef"}`},
		{http.MethodPatch, "/api/v1/nas/1", `{"allow_mab":true}`},
	} {
		rec := nasCall(t, mux, tc.method, tc.path, "noc_engineer", tc.body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s unconfigured: want 503, got %d", tc.method, tc.path, rec.Code)
		}
	}
}
