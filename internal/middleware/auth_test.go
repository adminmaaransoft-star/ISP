package middleware_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
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
