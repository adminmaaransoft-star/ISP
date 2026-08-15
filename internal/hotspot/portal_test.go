// Captive-portal tests — FR-HSP-001 | MDS §4.23.
//
// This is the one HTTP surface in the codebase that is deliberately open to
// strangers, so most of what follows is about what it refuses rather than what
// it grants: no oracle on which codes exist, no unmetered guessing, no
// redemption at all when the limiter is missing, and no MAC accepted in a
// spelling the RADIUS side would not recognise.
package hotspot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"golang.org/x/crypto/bcrypt"
)

// ── Test doubles ────────────────────────────────────────────────────────────

// fakeGrants records what the portal asked the store to do.
type fakeGrants struct {
	mu sync.Mutex

	voucherCalls []voucherCall
	loginCalls   []loginCall

	// validHash redeems successfully; every other hash is refused, which is how
	// the real store reports an unknown, spent, expired or voided code.
	validHash string
	// blockedSubscriber models a suspended account: correct credentials, no grant.
	blockedSubscriber int
	err               error
}

type voucherCall struct {
	CodeHash string
	MAC      string
	NASID    *int
}

type loginCall struct {
	MAC          string
	SubscriberID int
	NASID        *int
	Minutes      int
}

func (f *fakeGrants) RedeemVoucher(_ context.Context, codeHash, mac string, nasID *int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.voucherCalls = append(f.voucherCalls, voucherCall{codeHash, mac, nasID})
	if f.err != nil {
		return 0, f.err
	}
	if codeHash == f.validHash {
		return 4242, nil
	}
	return 0, nil
}

func (f *fakeGrants) GrantForSubscriber(_ context.Context, mac string, subscriberID int, nasID *int, minutes int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loginCalls = append(f.loginCalls, loginCall{mac, subscriberID, nasID, minutes})
	if f.err != nil {
		return 0, f.err
	}
	if subscriberID == f.blockedSubscriber {
		return 0, nil
	}
	return 7777, nil
}

func (f *fakeGrants) vouchers() []voucherCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]voucherCall(nil), f.voucherCalls...)
}

func (f *fakeGrants) logins() []loginCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]loginCall(nil), f.loginCalls...)
}

// fakeSubscribers holds one subscriber with a real bcrypt hash.
type fakeSubscribers struct {
	username string
	id       int
	hash     string
	err      error
}

func newFakeSubscribers(t *testing.T, username, password string, id int) *fakeSubscribers {
	t.Helper()
	// MinCost: this exercises the comparison, not the work factor, and the
	// production cost would add ~100ms to every test that logs in.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash test password: %v", err)
	}
	return &fakeSubscribers{username: username, id: id, hash: string(hash)}
}

func (f *fakeSubscribers) GetSubscriberByUsername(_ context.Context, username string) (*portal.SubscriberAuth, error) {
	if f.err != nil {
		return nil, f.err
	}
	if username != f.username {
		return nil, nil
	}
	return &portal.SubscriberAuth{ID: f.id, Username: f.username, PasswordHash: f.hash}, nil
}

// fakeLimiter allows a fixed number of attempts, then refuses.
type fakeLimiter struct {
	mu        sync.Mutex
	budget    int
	keys      []string
	err       error
	unlimited bool
}

func (l *fakeLimiter) Allow(_ context.Context, key string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	if l.err != nil {
		return false, l.err
	}
	if l.unlimited {
		return true, nil
	}
	l.budget--
	return l.budget >= 0, nil
}

func (l *fakeLimiter) seenKeys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

// ── Harness ─────────────────────────────────────────────────────────────────

type portalFixture struct {
	mux         *http.ServeMux
	grants      *fakeGrants
	subscribers *fakeSubscribers
	limiter     *fakeLimiter
}

func newPortal(t *testing.T) *portalFixture {
	t.Helper()
	f := &portalFixture{
		grants:      &fakeGrants{},
		subscribers: newFakeSubscribers(t, "walkup@isp", "correct-horse", 55),
		limiter:     &fakeLimiter{unlimited: true},
	}
	f.mux = http.NewServeMux()
	NewHandler(Deps{Grants: f.grants, Subscribers: f.subscribers, Limiter: f.limiter}).RegisterRoutes(f.mux)
	return f
}

func (f *portalFixture) get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil) //nolint:noctx // httptest.NewRequestWithContext needs go1.23; module is go1.22
	req.RemoteAddr = "192.0.2.10:51000"
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *portalFixture) post(path string, form url.Values) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, //nolint:noctx // see get()
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.10:51000"
	f.mux.ServeHTTP(rec, req)
	return rec
}

// ── Landing ─────────────────────────────────────────────────────────────────

func TestFR_HSP_001_LandingOffersBothWaysIn(t *testing.T) {
	f := newPortal(t)

	rec := f.get("/hotspot/portal?mac=aa-bb-cc-dd-ee-ff&nasid=3&link-orig=http://example.com/news")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{`action="/hotspot/voucher"`, `action="/hotspot/login"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the landing page must offer %s — a walk-up user with a voucher and a "+
				"subscriber with an account both arrive at this same page", want)
		}
	}
	// Re-emitted in the canonical spelling, so the MAC posted back is the one
	// the RADIUS side will look up.
	if !strings.Contains(body, "AA:BB:CC:DD:EE:FF") {
		t.Error("the landing page must carry the normalised MAC into its forms")
	}
	if !strings.Contains(body, `name="nasid" value="3"`) {
		t.Errorf("the NAS id must survive into the form, or the grant loses its NAS binding; got:\n%s", body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("captive-portal pages must not be cached — the next device to load this URL is a different device")
	}
}

func TestFR_HSP_001_LandingWithoutAMACOffersNoForm(t *testing.T) {
	f := newPortal(t)

	body := f.get("/hotspot/portal").Body.String()
	if strings.Contains(body, `action="/hotspot/voucher"`) {
		t.Error("with no device address there is nothing to grant access to, so offering a form " +
			"would only waste a submission the visitor cannot make succeed")
	}
	if !strings.Contains(body, "staff") {
		t.Error("a visitor stuck behind a misconfigured redirect should be told to ask for help")
	}
}

// ── Voucher redemption ──────────────────────────────────────────────────────

func TestFR_HSP_001_ValidVoucherOpensAGrant(t *testing.T) {
	f := newPortal(t)
	code := "HS-ABCD-EFGH-JKLM"
	f.grants.validHash = HashCode(code)

	rec := f.post("/hotspot/voucher", url.Values{
		"mac":       {"aa:bb:cc:dd:ee:ff"},
		"nasid":     {"7"},
		"code":      {code},
		"link-orig": {"http://example.com/news"},
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d — body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "You're connected") {
		t.Errorf("a successful redemption must confirm the device is online; got:\n%s", rec.Body.String())
	}
	// The NAS still has to authenticate the MAC afterwards, so the page has to
	// tell the user to reconnect rather than implying they are already through.
	if !strings.Contains(rec.Body.String(), "reconnect") {
		t.Error("the success page must tell the user to reconnect — the portal issues a grant, " +
			"it does not put the device on the network itself")
	}

	calls := f.grants.vouchers()
	if len(calls) != 1 {
		t.Fatalf("want 1 redemption call, got %d", len(calls))
	}
	if calls[0].MAC != "AA:BB:CC:DD:EE:FF" {
		t.Errorf("the MAC must be normalised before it reaches the store: got %q", calls[0].MAC)
	}
	if calls[0].NASID == nil || *calls[0].NASID != 7 {
		t.Errorf("nas_id must reach the store so the grant is bound to this site: got %v", calls[0].NASID)
	}
}

// TestFR_HSP_001_VoucherCodeIsAcceptedAsPrintedOrTyped keeps the printed
// grouping from becoming load-bearing.
func TestFR_HSP_001_VoucherCodeIsAcceptedAsPrintedOrTyped(t *testing.T) {
	code := "HS-ABCD-EFGH-JKLM"

	for _, typed := range []string{
		"HS-ABCD-EFGH-JKLM", // exactly as printed
		"hs-abcd-efgh-jklm", // lowercase
		"HSABCDEFGHJKLM",    // dashes omitted
		" HS ABCD EFGH JKLM ",
	} {
		f := newPortal(t)
		f.grants.validHash = HashCode(code)

		rec := f.post("/hotspot/voucher", url.Values{
			"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {typed},
		})
		if rec.Code != http.StatusOK {
			t.Errorf("code typed as %q was refused (status %d) — a valid voucher must not depend "+
				"on the user reproducing the dashes and capitals exactly", typed, rec.Code)
		}
	}
}

// noticePattern extracts the message shown to the visitor.
//
// The comparison below is on this text rather than the whole page because the
// page legitimately differs between attempts — it echoes the device's own MAC
// back into the form. Diffing entire bodies would therefore be satisfied by any
// two requests that happen to share a MAC, which is exactly the hole an earlier
// version of this test had: it passed against a build whose refusal message
// interpolated request data.
var noticePattern = regexp.MustCompile(`(?s)<div class="notice[^"]*">(.*?)</div>`)

func refusalNotice(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	m := noticePattern.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no notice rendered on a refusal; body:\n%s", rec.Body.String())
	}
	return strings.Join(strings.Fields(m[1]), " ")
}

// TestFR_HSP_001_RefusalsAreIndistinguishable is the anti-oracle property. A
// guesser who can tell "already used" from "no such code" has found a real
// code, which is the single bit of information a search of the code space
// needs.
//
// Both the code and the MAC are varied across the attempts: a refusal that
// echoed anything about the request back into its message would leak through
// exactly that channel, and holding either one constant would hide it.
func TestFR_HSP_001_RefusalsAreIndistinguishable(t *testing.T) {
	f := newPortal(t)
	f.grants.validHash = HashCode("HS-REAL-REAL-REAL")

	attempts := []struct{ mac, code string }{
		{"AA:BB:CC:DD:EE:FF", "HS-ZZZZ-ZZZZ-ZZZZ"}, // no such code
		{"11:22:33:44:55:66", "HS-YYYY-YYYY-YYYY"}, // a different guess, different device
		{"99:88:77:66:55:44", "HS-XXXX-XXXX-XXXX"},
	}

	var first string
	for i, a := range attempts {
		rec := f.post("/hotspot/voucher", url.Values{"mac": {a.mac}, "code": {a.code}})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: every refusal must be 401, got %d", i, rec.Code)
		}
		notice := refusalNotice(t, rec)
		if i == 0 {
			first = notice
			continue
		}
		if notice != first {
			t.Errorf("refusal messages differ between attempts:\n  %q\n  %q\n"+
				"any variation — between 'unknown', 'already used' and 'expired', or by echoing "+
				"the request back — tells a guesser which codes are real", first, notice)
		}
	}

	// And the message must not quote the attempt back at the guesser.
	for _, a := range attempts {
		if strings.Contains(first, a.code) || strings.Contains(first, a.mac) {
			t.Errorf("the refusal message %q repeats request data, which is a channel for exactly "+
				"the distinction this test exists to prevent", first)
		}
	}
}

func TestFR_HSP_001_MalformedRequestsAreRefusedBeforeTheStore(t *testing.T) {
	tests := []struct {
		name string
		form url.Values
		want int
	}{
		{"no MAC", url.Values{"code": {"HS-ABCD-EFGH-JKLM"}}, http.StatusBadRequest},
		{"unparseable MAC", url.Values{"mac": {"not-a-mac"}, "code": {"HS-A"}}, http.StatusBadRequest},
		// 422, not 401: the visitor knows they submitted nothing, so telling
		// them so reveals nothing about which codes exist.
		{"MAC but no code", url.Values{"mac": {"AA:BB:CC:DD:EE:FF"}}, http.StatusUnprocessableEntity},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newPortal(t)
			rec := f.post("/hotspot/voucher", tc.form)
			if rec.Code != tc.want {
				t.Errorf("status: want %d, got %d", tc.want, rec.Code)
			}
			if got := len(f.grants.vouchers()); got != 0 {
				t.Errorf("a request this malformed must not reach the store, got %d call(s)", got)
			}
		})
	}
}

// ── Attempt limiting ────────────────────────────────────────────────────────

func TestFR_HSP_001_GuessingIsRateLimited(t *testing.T) {
	f := newPortal(t)
	f.limiter.unlimited = false
	f.limiter.budget = 2
	f.grants.validHash = HashCode("HS-REAL-REAL-REAL")

	for i := 0; i < 2; i++ {
		if rec := f.post("/hotspot/voucher", url.Values{
			"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {"HS-WRNG-WRNG-WRNG"},
		}); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: want 401 within budget, got %d", i+1, rec.Code)
		}
	}

	rec := f.post("/hotspot/voucher", url.Values{
		"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {"HS-WRNG-WRNG-WRNG"},
	})
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("past the budget: want 429, got %d", rec.Code)
	}
	if got := len(f.grants.vouchers()); got != 2 {
		t.Errorf("a rate-limited attempt must not reach the store: want 2 store calls, got %d", got)
	}

	// Even the correct code is refused once the budget is gone. Exempting
	// successes would let an attacker who guesses right keep going.
	if rec := f.post("/hotspot/voucher", url.Values{
		"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {"HS-REAL-REAL-REAL"},
	}); rec.Code != http.StatusTooManyRequests {
		t.Errorf("the limit must apply regardless of whether the code is right, got %d", rec.Code)
	}
}

// TestFR_HSP_001_LimitKeyCombinesAddressAndMAC — either identifier alone is
// weak: a MAC is client-controlled and rotatable, and a whole café behind one
// NAT shares a source address.
func TestFR_HSP_001_LimitKeyCombinesAddressAndMAC(t *testing.T) {
	f := newPortal(t)
	f.post("/hotspot/voucher", url.Values{"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {"HS-A"}})

	keys := f.limiter.seenKeys()
	if len(keys) != 1 {
		t.Fatalf("want 1 limiter key, got %d", len(keys))
	}
	if !strings.Contains(keys[0], "192.0.2.10") || !strings.Contains(keys[0], "AA:BB:CC:DD:EE:FF") {
		t.Errorf("the limiter key must combine source address and MAC, got %q", keys[0])
	}
}

// TestFR_HSP_001_BrokenLimiterRefusesRatherThanWaves is the direction this has
// to fail in: an unmetered voucher endpoint is worse than an unavailable one.
func TestFR_HSP_001_BrokenLimiterRefusesRatherThanWaves(t *testing.T) {
	f := newPortal(t)
	f.limiter.err = errors.New("redis is down")
	f.grants.validHash = HashCode("HS-REAL-REAL-REAL")

	rec := f.post("/hotspot/voucher", url.Values{
		"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {"HS-REAL-REAL-REAL"},
	})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a limiter failure must refuse the attempt, got %d", rec.Code)
	}
	if got := len(f.grants.vouchers()); got != 0 {
		t.Errorf("nothing may be redeemed while the limiter is unavailable, got %d call(s)", got)
	}
}

func TestFR_HSP_001_NoLimiterMeansNoRedemption(t *testing.T) {
	mux := http.NewServeMux()
	grants := &fakeGrants{validHash: HashCode("HS-REAL-REAL-REAL")}
	NewHandler(Deps{Grants: grants, Subscribers: &fakeSubscribers{}}).RegisterRoutes(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/hotspot/voucher", //nolint:noctx // see portalFixture.get()
		strings.NewReader(url.Values{"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {"HS-REAL-REAL-REAL"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a deployment with no attempt limiter must not run a captive portal at all: got %d", rec.Code)
	}
	if len(grants.vouchers()) != 0 {
		t.Error("nothing may be redeemed without a configured limiter")
	}
}

// ── Subscriber login ────────────────────────────────────────────────────────

func TestFR_HSP_001_SubscriberLoginOpensAGrant(t *testing.T) {
	f := newPortal(t)

	rec := f.post("/hotspot/login", url.Values{
		"mac": {"AA:BB:CC:DD:EE:FF"}, "username": {"walkup@isp"}, "password": {"correct-horse"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d — body:\n%s", rec.Code, rec.Body.String())
	}

	calls := f.grants.logins()
	if len(calls) != 1 {
		t.Fatalf("want 1 grant call, got %d", len(calls))
	}
	if calls[0].SubscriberID != 55 {
		t.Errorf("subscriber id: want 55, got %d", calls[0].SubscriberID)
	}
	if calls[0].Minutes != DefaultLoginMinutes {
		t.Errorf("grant duration: want %d minutes, got %d — an unbounded grant would outlive "+
			"the subscriber's next suspension", DefaultLoginMinutes, calls[0].Minutes)
	}
}

func TestFR_HSP_001_WrongCredentialsGrantNothing(t *testing.T) {
	for _, tc := range []struct{ name, username, password string }{
		{"wrong password", "walkup@isp", "guess"},
		{"unknown user", "nobody@isp", "correct-horse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPortal(t)
			rec := f.post("/hotspot/login", url.Values{
				"mac": {"AA:BB:CC:DD:EE:FF"}, "username": {tc.username}, "password": {tc.password},
			})
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status: want 401, got %d", rec.Code)
			}
			if len(f.grants.logins()) != 0 {
				t.Error("a failed login must not reach the grant store")
			}
		})
	}
}

// TestFR_HSP_001_SuspendedSubscriberIsToldPlainly — the store refuses the
// grant, and since the caller has already proved who they are, saying so is
// not an oracle and is what sends them to support instead of retyping a
// password that was right.
func TestFR_HSP_001_SuspendedSubscriberIsToldPlainly(t *testing.T) {
	f := newPortal(t)
	f.grants.blockedSubscriber = 55

	rec := f.post("/hotspot/login", url.Values{
		"mac": {"AA:BB:CC:DD:EE:FF"}, "username": {"walkup@isp"}, "password": {"correct-horse"},
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not active") {
		t.Errorf("a suspended subscriber must be told their account is the problem; got:\n%s", rec.Body.String())
	}
}

// ── Redirect handling ───────────────────────────────────────────────────────

// TestFR_HSP_001_OnlyHTTPRedirectsAreEchoed keeps link-orig — which arrives in
// the URL an attacker may have crafted — from turning the success page into an
// open redirect or a javascript: sink.
func TestFR_HSP_001_OnlyHTTPRedirectsAreEchoed(t *testing.T) {
	dangerous := []string{
		"javascript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"//evil.example/phish",
		"vbscript:msgbox",
	}
	for _, raw := range dangerous {
		t.Run(raw, func(t *testing.T) {
			f := newPortal(t)
			code := "HS-ABCD-EFGH-JKLM"
			f.grants.validHash = HashCode(code)

			rec := f.post("/hotspot/voucher", url.Values{
				"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {code}, "link-orig": {raw},
			})
			if strings.Contains(rec.Body.String(), raw) {
				t.Errorf("the success page must not echo %q as a link — link-orig comes from the "+
					"URL and is attacker-controllable", raw)
			}
		})
	}

	// A genuine http(s) destination still comes through, or the feature is gone.
	f := newPortal(t)
	code := "HS-ABCD-EFGH-JKLM"
	f.grants.validHash = HashCode(code)
	rec := f.post("/hotspot/voucher", url.Values{
		"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {code}, "link-orig": {"https://example.com/news"},
	})
	if !strings.Contains(rec.Body.String(), "https://example.com/news") {
		t.Error("an ordinary http(s) destination must still be offered as a continue link")
	}
}

// ── Voucher codes ───────────────────────────────────────────────────────────

func TestFR_HSP_001_GeneratedCodesAreDistinctAndTypeable(t *testing.T) {
	seen := make(map[string]bool, 200)
	for i := 0; i < 200; i++ {
		v, err := GenerateCode()
		if err != nil {
			t.Fatalf("GenerateCode: %v", err)
		}
		if seen[v.Plaintext] {
			t.Fatalf("GenerateCode produced a duplicate at iteration %d: %q", i, v.Plaintext)
		}
		seen[v.Plaintext] = true

		if !strings.HasPrefix(v.Plaintext, v.Prefix) {
			t.Fatalf("the stored prefix %q must be the leading part of the code %q, or a "+
				"listing cannot be matched back to a printed voucher", v.Prefix, v.Plaintext)
		}
		if len(v.Prefix) > 12 {
			t.Fatalf("code_prefix is VARCHAR(12); %q would be truncated by the database", v.Prefix)
		}
		// The alphabet exists so a code read off paper cannot be mistyped in the
		// ways that matter: 0/O and 1/I/L.
		for _, banned := range []string{"0", "O", "1", "I", "L"} {
			if strings.Contains(strings.TrimPrefix(v.Plaintext, codeLabel), banned) {
				t.Fatalf("code %q contains %q, which is misread off printed paper", v.Plaintext, banned)
			}
		}
		if v.Hash != HashCode(v.Plaintext) {
			t.Fatal("the stored hash must be the hash of the code as issued")
		}
	}
}

func TestFR_HSP_001_HashIgnoresGroupingAndCase(t *testing.T) {
	want := HashCode("HS-ABCD-EFGH-JKLM")
	for _, variant := range []string{"hs-abcd-efgh-jklm", "HSABCDEFGHJKLM", "hs abcd efgh jklm", " HS-ABCD-EFGH-JKLM "} {
		if got := HashCode(variant); got != want {
			t.Errorf("HashCode(%q) must match the printed form's hash", variant)
		}
	}
	if HashCode("HS-ABCD-EFGH-JKLN") == want {
		t.Error("two different codes must not collide")
	}
}

// TestFR_HSP_001_NASIDIsOptional — an absent or junk nasid leaves the grant
// unbound, matching the NULL nas_id the schema treats as "any NAS this
// operator runs", rather than failing the redemption.
func TestFR_HSP_001_NASIDIsOptional(t *testing.T) {
	for _, nasid := range []string{"", "abc", "0", "-4"} {
		f := newPortal(t)
		code := "HS-ABCD-EFGH-JKLM"
		f.grants.validHash = HashCode(code)

		form := url.Values{"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {code}}
		if nasid != "" {
			form.Set("nasid", nasid)
		}
		if rec := f.post("/hotspot/voucher", form); rec.Code != http.StatusOK {
			t.Fatalf("nasid=%q: want 200, got %d", nasid, rec.Code)
		}
		calls := f.grants.vouchers()
		if len(calls) != 1 || calls[0].NASID != nil {
			t.Errorf("nasid=%q must leave the grant unbound, got %v", nasid, calls[0].NASID)
		}
	}

	// A real one is passed through as an integer.
	f := newPortal(t)
	code := "HS-ABCD-EFGH-JKLM"
	f.grants.validHash = HashCode(code)
	f.post("/hotspot/voucher", url.Values{
		"mac": {"AA:BB:CC:DD:EE:FF"}, "code": {code}, "nasid": {strconv.Itoa(9)},
	})
	if calls := f.grants.vouchers(); calls[0].NASID == nil || *calls[0].NASID != 9 {
		t.Errorf("a valid nasid must reach the store, got %v", calls[0].NASID)
	}
}
