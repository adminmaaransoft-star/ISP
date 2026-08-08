package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

const testSecret = "test_jwt_secret_32_chars_minimum!!"

func makeToken(t *testing.T, role string, expiresAt time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{
		"role": role,
		"sub":  "test_user",
		"exp":  expiresAt.Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

// makeTokenWithClaims is makeToken's more general form, for tests that need
// claims beyond role/sub/exp (e.g. lea_access).
func makeTokenWithClaims(t *testing.T, extra map[string]any, expiresAt time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{"sub": "test_user", "exp": expiresAt.Unix()}
	for k, v := range extra {
		claims[k] = v
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}

// TestJWTMiddleware_ValidToken verifies that a valid token passes through.
func TestJWTMiddleware_ValidToken(t *testing.T) {
	token := makeToken(t, "billing_admin", time.Now().Add(time.Hour))

	handler := middleware.JWTMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

// TestJWTMiddleware_MissingToken verifies that a missing token returns 401.
func TestJWTMiddleware_MissingToken(t *testing.T) {
	handler := middleware.JWTMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rr.Code)
	}
}

// TestJWTMiddleware_ExpiredToken verifies that an expired token returns 401.
func TestJWTMiddleware_ExpiredToken(t *testing.T) {
	token := makeToken(t, "isp_owner", time.Now().Add(-time.Hour)) // expired

	handler := middleware.JWTMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for expired token, got %d", rr.Code)
	}
}

// TestRequireRole_AllowedRole verifies that the correct role passes through.
func TestRequireRole_AllowedRole(t *testing.T) {
	token := makeToken(t, "noc_engineer", time.Now().Add(time.Hour))

	handler := middleware.JWTMiddleware(testSecret)(
		middleware.RequireRole("noc_engineer", "isp_owner")(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for allowed role, got %d", rr.Code)
	}
}

// TestRequireRole_ForbiddenRole verifies that an unauthorised role returns 403.
func TestRequireRole_ForbiddenRole(t *testing.T) {
	token := makeToken(t, "subscriber", time.Now().Add(time.Hour))

	handler := middleware.JWTMiddleware(testSecret)(
		middleware.RequireRole("noc_engineer", "billing_admin")(
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}),
		),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 for forbidden role, got %d", rr.Code)
	}
}

// TestSubjectFromContext verifies the JWT's "sub" claim is retrievable from
// the request context after JWTMiddleware runs.
func TestSubjectFromContext(t *testing.T) {
	token := makeToken(t, "billing_admin", time.Now().Add(time.Hour))

	var got string
	handler := middleware.JWTMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = middleware.SubjectFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got != "test_user" {
		t.Errorf("SubjectFromContext: want %q, got %q", "test_user", got)
	}
}

// TestLeaAccessFromContext verifies the lea_access claim round-trips through
// the context both when present and when absent — SecD §9.3 requires LEA
// authorization to check this independently of role.
func TestLeaAccessFromContext(t *testing.T) {
	tests := []struct {
		name       string
		setClaim   bool
		leaAccess  bool
		wantResult bool
	}{
		{"true when the claim is set", true, true, true},
		{"false when the claim is explicitly false", true, false, false},
		{"false when the claim is entirely absent", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{"role": "noc_engineer"}
			if tc.setClaim {
				extra["lea_access"] = tc.leaAccess
			}
			token := makeTokenWithClaims(t, extra, time.Now().Add(time.Hour))

			var got bool
			handler := middleware.JWTMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = middleware.LeaAccessFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
			req.Header.Set("Authorization", "Bearer "+token)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if got != tc.wantResult {
				t.Errorf("LeaAccessFromContext: want %v, got %v", tc.wantResult, got)
			}
		})
	}
}

// TestRequireLeaAccess_Allowed verifies a noc_engineer token that also
// carries lea_access=true passes through.
func TestRequireLeaAccess_Allowed(t *testing.T) {
	token := makeTokenWithClaims(t, map[string]any{"role": "noc_engineer", "lea_access": true}, time.Now().Add(time.Hour))

	handler := middleware.JWTMiddleware(testSecret)(
		middleware.RequireRole("noc_engineer")(
			middleware.RequireLeaAccess(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})),
		),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("want 200 with lea_access=true, got %d", rr.Code)
	}
}

// TestRequireLeaAccess_ForbiddenWithoutClaim verifies the SecD §9.3
// guarantee directly: a correct role alone is not enough — the lea_access
// claim must also be present and true, independent of role.
func TestRequireLeaAccess_ForbiddenWithoutClaim(t *testing.T) {
	token := makeToken(t, "noc_engineer", time.Now().Add(time.Hour)) // no lea_access claim at all

	handler := middleware.JWTMiddleware(testSecret)(
		middleware.RequireRole("noc_engineer")(
			middleware.RequireLeaAccess(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})),
		),
	)

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("want 403 without the lea_access claim, got %d", rr.Code)
	}
}

// withCapturedLog temporarily redirects the package-level zerolog logger to
// buf so a test can assert on what was logged, restoring it afterward.
func withCapturedLog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	orig := log.Logger
	log.Logger = zerolog.New(buf)
	t.Cleanup(func() { log.Logger = orig })
}

// TestAudit_EmitsStructuredLogLine verifies Audit's log line carries actor
// identity/role (from context), the action/target the caller passed, a
// non-empty correlation_id, and the detail map — the full SecD §9.3 audit
// record shape.
func TestAudit_EmitsStructuredLogLine(t *testing.T) {
	var buf bytes.Buffer
	withCapturedLog(t, &buf)

	token := makeToken(t, "billing_admin", time.Now().Add(time.Hour))
	handler := middleware.JWTMiddleware(testSecret)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		middleware.Audit(r.Context(), "ticket.create", "ticket:42", map[string]any{"category": "billing"})
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil) //nolint:noctx
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse audit log line: %v (raw: %s)", err, buf.String())
	}
	if entry["audit"] != true {
		t.Errorf("audit: want true, got %v", entry["audit"])
	}
	if entry["action"] != "ticket.create" {
		t.Errorf("action: want %q, got %v", "ticket.create", entry["action"])
	}
	if entry["target"] != "ticket:42" {
		t.Errorf("target: want %q, got %v", "ticket:42", entry["target"])
	}
	if entry["actor_id"] != "test_user" {
		t.Errorf("actor_id: want %q, got %v", "test_user", entry["actor_id"])
	}
	if entry["actor_role"] != "billing_admin" {
		t.Errorf("actor_role: want %q, got %v", "billing_admin", entry["actor_role"])
	}
	if cid, _ := entry["correlation_id"].(string); cid == "" {
		t.Error("correlation_id must be a non-empty string")
	}
	detail, ok := entry["detail"].(map[string]any)
	if !ok {
		t.Fatalf("detail field missing or wrong type: %v", entry["detail"])
	}
	if detail["category"] != "billing" {
		t.Errorf("detail.category: want %q, got %v", "billing", detail["category"])
	}
}

// TestAudit_OmitsDetailWhenEmpty verifies an empty/nil detail map is left
// out of the log line entirely rather than logged as null or {}, and
// exercises Audit with no subject/role in context (the zero-value case).
func TestAudit_OmitsDetailWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	withCapturedLog(t, &buf)

	middleware.Audit(context.Background(), "system.startup", "daemon", nil)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("parse audit log line: %v (raw: %s)", err, buf.String())
	}
	if _, exists := entry["detail"]; exists {
		t.Error("detail field must be omitted when the detail map is empty/nil")
	}
	if entry["actor_id"] != "" {
		t.Errorf("actor_id: want empty string with no auth context, got %v", entry["actor_id"])
	}
}
