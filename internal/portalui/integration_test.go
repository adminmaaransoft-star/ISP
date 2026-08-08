package portalui_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/portalui"
)

// newCombinedMux mirrors cmd/api/main.go's wiring: the JSON portal API and
// the browsable portal UI registered on the same mux, sharing the same JWT
// secret — the same topology this cookie-isolation guarantee has to hold
// under in production, not just in isolation.
func newCombinedMux(t *testing.T) *http.ServeMux {
	t.Helper()
	const secret = "test-portal-secret"

	jsonHandler := portal.NewHandler(&stubSubscribers{}, &stubSessionsOnline{}, nil, nil, nil, secret)
	uiHandler := portalui.NewHandler(portalui.Deps{
		Subscribers: &stubSubscribers{},
		Sessions:    &stubSessionsOnline{},
		JWTSecret:   secret,
	})

	mux := http.NewServeMux()
	jsonHandler.RegisterRoutes(mux)
	uiHandler.RegisterRoutes(mux)
	return mux
}

// TestJSONAPI_RejectsPortalSessionCookie is the regression test the plan
// called out explicitly: a portal_session cookie (scoped Path=/ui, meant
// only for the browsable UI) must never be accepted as credentials by the
// JSON /portal/* routes. Those routes are Bearer-only by design — this test
// exists so that guarantee can't silently erode in a future refactor without
// a test failing.
func TestJSONAPI_RejectsPortalSessionCookie(t *testing.T) {
	mux := newCombinedMux(t)

	got := loginAndGetCookie(t, mux, "testuser", "testpass")
	if got.Cookie == nil {
		t.Fatalf("login did not return a session cookie (status %d)", got.Status)
	}

	form := url.Values{"category": {"connectivity"}, "description": {"test"}}
	req := httptest.NewRequest("POST", "/portal/tickets", strings.NewReader(form.Encode())) //nolint:noctx
	req.AddCookie(got.Cookie)                                                               // deliberately no Authorization header
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("JSON API must reject a portal_session cookie with no Authorization header; want 401, got %d — %s",
			rec.Code, rec.Body.String())
	}
}

// TestUICookie_NotSentToJSONAPI confirms the cookie's own Path scoping (not
// just server-side rejection) is what a real browser would enforce — the
// belt-and-suspenders half of the same guarantee.
func TestUICookie_NotSentToJSONAPI(t *testing.T) {
	mux := newCombinedMux(t)
	got := loginAndGetCookie(t, mux, "testuser", "testpass")
	if got.Cookie == nil {
		t.Fatal("login did not return a session cookie")
	}
	if got.Cookie.Path != "/ui" {
		t.Fatalf("portal_session cookie Path = %q, want /ui so browsers never send it to /portal/* or /api/v1/*", got.Cookie.Path)
	}
}
