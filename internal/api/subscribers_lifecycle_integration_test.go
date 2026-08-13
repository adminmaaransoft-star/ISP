//go:build integration

// Subscriber lifecycle endpoint tests — FR-LC-001..003, FR-BIL-010..011 |
// MDS §4.14.
//
// Each handler is exercised through the real middleware chain against
// in-memory stubs of its store dependencies (same shape as
// new_endpoints_integration_test.go), so what is under test is route wiring,
// authorization, proration/CoA/PoD side effects and response shape — the SQL
// itself is covered in internal/db/subscribers_integration_test.go and
// internal/db/billing_integration_test.go.
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/middleware"
	"github.com/shopspring/decimal"
)

// itRoleTokenWithSubject mints a role token carrying a username in the
// Subject claim — itRoleToken (new_endpoints_integration_test.go) leaves
// Subject unset, which is fine for tests that only check authorization, but
// staff attribution (adjustedBy/refundedBy) reads SubjectFromContext, so a
// test asserting on that value needs a token shaped like the real ones
// staffui.auth issues (Subject: username), not a bare role token.
func itRoleTokenWithSubject(t *testing.T, role, subject string) string {
	t.Helper()
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		Role:             role,
		RegisteredClaims: jwt.RegisteredClaims{Subject: subject, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	}).SignedString([]byte(itJWTSecret))
	if err != nil {
		t.Fatalf("sign %s token: %v", role, err)
	}
	return tok
}

// ── Stubs ────────────────────────────────────────────────────────────────────

type stubLifecycle struct {
	info    *api.PlanChangeInfo
	infoErr error

	changed   *api.SubscriberRecord
	changeErr error

	terminated   *api.SubscriberRecord
	terminateErr error

	lastNewPlanID int
	lastNewExpiry time.Time
}

func (s *stubLifecycle) GetPlanChangeInfo(_ context.Context, _, newPlanID int) (*api.PlanChangeInfo, error) {
	if s.infoErr != nil {
		return nil, s.infoErr
	}
	return s.info, nil
}

func (s *stubLifecycle) SetSubscriberPlan(_ context.Context, id, newPlanID int, newExpiry time.Time) (*api.SubscriberRecord, error) {
	s.lastNewPlanID, s.lastNewExpiry = newPlanID, newExpiry
	if s.changeErr != nil {
		return nil, s.changeErr
	}
	if s.changed != nil {
		return s.changed, nil
	}
	return &api.SubscriberRecord{ID: id, Username: "changed_sub", PlanID: newPlanID, PlanExpiry: &newExpiry}, nil
}

func (s *stubLifecycle) TerminateSubscriber(_ context.Context, id int) (*api.SubscriberRecord, error) {
	if s.terminateErr != nil {
		return nil, s.terminateErr
	}
	if s.terminated != nil {
		return s.terminated, nil
	}
	return &api.SubscriberRecord{ID: id, Username: "terminated_sub", Status: "terminated"}, nil
}

// stubRefunds implements api.RefundQuerier.
type stubRefunds struct {
	lastSubscriberID, lastLedgerEntryID int
	lastAmount                          decimal.Decimal
	lastReason, lastRefundedBy          string
	err                                 error
}

func (s *stubRefunds) CreateRefund(_ context.Context, subscriberID, ledgerEntryID int, amount decimal.Decimal, reason, refundedBy string) (int, error) {
	s.lastSubscriberID, s.lastLedgerEntryID = subscriberID, ledgerEntryID
	s.lastAmount, s.lastReason, s.lastRefundedBy = amount, reason, refundedBy
	if s.err != nil {
		return 0, s.err
	}
	return 42, nil
}

// stubSubCache implements api.SubscriberCacheInvalidator.
type stubSubCache struct {
	invalidated []string
}

func (s *stubSubCache) InvalidateSubscriber(_ context.Context, username string) error {
	s.invalidated = append(s.invalidated, username)
	return nil
}

// stubWalletFunded is stubWallet with a caller-chosen starting balance, so a
// debit (adjustment, refund, auto-renewal path) can be tested both when it
// fits and when it does not — stubWallet's fixed zero balance can only ever
// exercise the "insufficient" side.
type stubWalletFunded struct {
	balance           decimal.Decimal
	rechargeIDCounter int
}

func (s *stubWalletFunded) GetTransactionByToken(context.Context, string) (*billing.Transaction, error) {
	return nil, nil
}
func (s *stubWalletFunded) GetSubscriberBalance(context.Context, int) (decimal.Decimal, error) {
	return s.balance, nil
}
func (s *stubWalletFunded) RecordRecharge(_ context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	s.rechargeIDCounter++
	return &billing.Transaction{
		ID: s.rechargeIDCounter, SubscriberID: p.Credit.SubscriberID,
		EntryType: p.Credit.EntryType, Amount: p.Credit.Amount, BalanceAfter: p.NewBalance,
	}, nil
}

// ── Plan change (FR-LC-001) ─────────────────────────────────────────────────

func TestChangeSubscriberPlan_ProratesAndPersists(t *testing.T) {
	expiry := time.Now().Add(10 * 24 * time.Hour)
	lc := &stubLifecycle{info: &api.PlanChangeInfo{
		Username: "prorate_sub", CurrentExpiry: &expiry,
		OldPrice: decimal.NewFromInt(300), OldValidityDays: 30,
		NewPrice: decimal.NewFromInt(600), NewValidityDays: 30,
	}}
	cache := &stubSubCache{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc, SubCache: cache,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if lc.lastNewPlanID != 5 {
		t.Errorf("new_plan_id passed to store = %d, want 5", lc.lastNewPlanID)
	}
	// 10 days remaining at 10/day old value = 100 credit; new plan is 20/day,
	// so 5 bonus days on top of the new plan's own 30 = 35 total.
	wantMin := time.Now().Add(34 * 24 * time.Hour)
	wantMax := time.Now().Add(36 * 24 * time.Hour)
	if lc.lastNewExpiry.Before(wantMin) || lc.lastNewExpiry.After(wantMax) {
		t.Errorf("new_expiry = %v, want roughly 35 days out (between %v and %v)", lc.lastNewExpiry, wantMin, wantMax)
	}
	if len(cache.invalidated) != 1 || cache.invalidated[0] != "prorate_sub" {
		t.Errorf("auth-cache invalidation = %v, want exactly [prorate_sub]", cache.invalidated)
	}
}

func TestChangeSubscriberPlan_UnknownSubscriber_404(t *testing.T) {
	lc := &stubLifecycle{info: nil}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Lifecycle: lc,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/404/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestChangeSubscriberPlan_UnknownPlan_422(t *testing.T) {
	lc := &stubLifecycle{infoErr: api.ErrInvalidPlan}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), Lifecycle: lc,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":999999}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an unknown new_plan_id, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestChangeSubscriberPlan_NoActiveSession_NoCoAEnqueued verifies a
// subscriber with no live session gets no CoA — there is nothing to
// reconfigure, and enqueueing one would just be a task that fails to find a
// session when it runs.
func TestChangeSubscriberPlan_NoActiveSession_NoCoAEnqueued(t *testing.T) {
	lc := &stubLifecycle{info: &api.PlanChangeInfo{Username: "u", OldValidityDays: 30, NewValidityDays: 30}}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc, Sessions: &stubSessionReader{session: nil}, Tasks: tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	if len(tasks.snapshot()) != 0 {
		t.Errorf("no session active: want 0 enqueued tasks, got %d", len(tasks.snapshot()))
	}
}

// TestChangeSubscriberPlan_ActiveSession_EnqueuesCoA is the FR-AAA-007 closure
// this endpoint exists for: a live session must get a CoA so the new rate
// limit applies without waiting for reauthentication.
func TestChangeSubscriberPlan_ActiveSession_EnqueuesCoA(t *testing.T) {
	lc := &stubLifecycle{info: &api.PlanChangeInfo{Username: "u", OldValidityDays: 30, NewValidityDays: 30}}
	tasks := &stubTaskEnqueuer{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc,
		Sessions:  &stubSessionReader{session: &health.SessionSummary{NasIP: "10.0.0.5"}},
		Tasks:     tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"new_plan_id":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/plan-change", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	got := tasks.snapshot()
	if len(got) != 1 || got[0].Type() != "network:coa_send" {
		t.Fatalf("want exactly 1 network:coa_send task, got %+v", got)
	}
}

// ── Termination (FR-LC-002) ─────────────────────────────────────────────────

func TestTerminateSubscriber_SetsStatusAndEnqueuesPoD(t *testing.T) {
	lc := &stubLifecycle{}
	tasks := &stubTaskEnqueuer{}
	cache := &stubSubCache{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: lc, SubCache: cache,
		Sessions: &stubSessionReader{session: &health.SessionSummary{NasIP: "10.0.0.5"}},
		Tasks:    tasks,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/terminate", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d — %s", rec.Code, rec.Body.String())
	}
	var got api.SubscriberRecord
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "terminated" {
		t.Errorf("status = %q, want terminated", got.Status)
	}
	tasksGot := tasks.snapshot()
	if len(tasksGot) != 1 || tasksGot[0].Type() != "network:pod_send" {
		t.Fatalf("want exactly 1 network:pod_send task, got %+v", tasksGot)
	}
	if len(cache.invalidated) != 1 || cache.invalidated[0] != "terminated_sub" {
		t.Errorf("auth-cache invalidation = %v, want exactly [terminated_sub]", cache.invalidated)
	}
}

func TestTerminateSubscriber_UnknownSubscriber_404(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: &stubLifecycleAlwaysNotFound{},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/404/terminate", nil)
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("want 404, got %d — %s", rec.Code, rec.Body.String())
	}
}

// stubLifecycleAlwaysNotFound reports every lookup as a missing subscriber —
// TerminateSubscriber's (nil, nil) convention needs a dedicated stub since
// stubLifecycle's zero value instead returns a synthesized record.
type stubLifecycleAlwaysNotFound struct{}

func (s *stubLifecycleAlwaysNotFound) GetPlanChangeInfo(context.Context, int, int) (*api.PlanChangeInfo, error) {
	return nil, nil
}
func (s *stubLifecycleAlwaysNotFound) SetSubscriberPlan(context.Context, int, int, time.Time) (*api.SubscriberRecord, error) {
	return nil, nil
}
func (s *stubLifecycleAlwaysNotFound) TerminateSubscriber(context.Context, int) (*api.SubscriberRecord, error) {
	return nil, nil
}

// ── Adjustments (FR-BIL-010) ────────────────────────────────────────────────

func TestCreateAdjustment_CreditSucceeds(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWalletFunded{balance: decimal.NewFromInt(100)}),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"credit","reason":"goodwill credit"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestCreateAdjustment_DebitExceedsBalance_422 is the negative control for
// the overdraft guard: a debit larger than the wallet holds must be refused,
// not silently taken negative.
func TestCreateAdjustment_DebitExceedsBalance_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), // balance always 0
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"debit","reason":"correction"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for a debit exceeding balance, got %d — %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAdjustment_MissingReason_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWalletFunded{balance: decimal.NewFromInt(100)}),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"credit","reason":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for a missing reason, got %d", rec.Code)
	}
}

func TestCreateAdjustment_InvalidDirection_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWalletFunded{balance: decimal.NewFromInt(100)}),
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"50.00","direction":"sideways","reason":"x"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/adjustments", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for an invalid direction, got %d", rec.Code)
	}
}

// ── Refunds (FR-BIL-011) ─────────────────────────────────────────────────────

func TestCreateRefund_Succeeds(t *testing.T) {
	refunds := &stubRefunds{}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWalletFunded{balance: decimal.NewFromInt(500)}),
		Refunds: refunds,
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"200.00","reason":"duplicate recharge"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/refunds", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleTokenWithSubject(t, "billing_admin", "priya.billing"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d — %s", rec.Code, rec.Body.String())
	}
	if refunds.lastSubscriberID != 9 || !refunds.lastAmount.Equal(decimal.NewFromInt(200)) {
		t.Errorf("refund record: subscriber=%d amount=%s, want 9 and 200", refunds.lastSubscriberID, refunds.lastAmount)
	}
	if refunds.lastRefundedBy != "priya.billing" {
		t.Errorf("refund record staff attribution: want %q, got %q", "priya.billing", refunds.lastRefundedBy)
	}
}

// TestCreateRefund_ExceedsBalance_422 verifies a refund cannot take the
// wallet negative — the same overdraft guard as adjustments, applied to
// refunds since both go through WalletService.Post.
func TestCreateRefund_ExceedsBalance_422(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}), // balance always 0
		Refunds: &stubRefunds{},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"200.00","reason":"duplicate recharge"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/refunds", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("want 422 for a refund exceeding balance, got %d — %s", rec.Code, rec.Body.String())
	}
}

// TestCreateRefund_RecordFailsAfterWalletDebit is the negative control for
// the "money moved, log the rest" pattern (MDS §4.14): the wallet debit must
// not be undone just because the payment_refunds row failed to write, since
// the ledger leg is itself a true record of the money movement.
func TestCreateRefund_RecordFailsAfterWalletDebit(t *testing.T) {
	wallet := &stubWalletFunded{balance: decimal.NewFromInt(500)}
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(wallet),
		Refunds: &stubRefunds{err: context.DeadlineExceeded},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	body := `{"amount":"200.00","reason":"duplicate recharge"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/subscribers/9/refunds", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the refund record fails, got %d — %s", rec.Code, rec.Body.String())
	}
	if wallet.rechargeIDCounter != 1 {
		t.Error("the wallet debit must still have been written even though the refund record failed")
	}
}

// ── Authorization and configuration ─────────────────────────────────────────

// TestLifecycleRoutes_ForbiddenForCSR verifies these money/lifecycle-mutating
// routes are restricted to billing_admin/isp_owner, the same gate as
// PATCH /subscribers/{id} and POST /wallets/recharge.
func TestLifecycleRoutes_ForbiddenForCSR(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		Lifecycle: &stubLifecycle{info: &api.PlanChangeInfo{OldValidityDays: 30, NewValidityDays: 30}},
		Refunds:   &stubRefunds{},
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	reqs := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/subscribers/9/plan-change", `{"new_plan_id":1}`},
		{http.MethodPost, "/api/v1/subscribers/9/terminate", ``},
		{http.MethodPost, "/api/v1/subscribers/9/adjustments", `{"amount":"10","direction":"credit","reason":"x"}`},
		{http.MethodPost, "/api/v1/subscribers/9/refunds", `{"amount":"10","reason":"x"}`},
	}
	for _, tc := range reqs {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "csr", false))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: want 403 for csr, got %d", tc.path, rec.Code)
		}
	}
}

func TestLifecycleRoutes_UnconfiguredStoreReturns503(t *testing.T) {
	h := api.NewHandler(api.HandlerDeps{
		DB: &stubDB{}, KYC: &stubKYC{}, Wallet: billing.NewWalletService(&stubWallet{}),
		// Lifecycle and Refunds left nil.
	})
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, itJWTSecret)

	reqs := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/subscribers/9/plan-change", `{"new_plan_id":1}`},
		{http.MethodPost, "/api/v1/subscribers/9/terminate", ``},
		{http.MethodPost, "/api/v1/subscribers/9/refunds", `{"amount":"10","reason":"x"}`},
	}
	for _, tc := range reqs {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+itRoleToken(t, "billing_admin", false))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: want 503 when unconfigured, got %d — %s", tc.path, rec.Code, rec.Body.String())
		}
	}
}
