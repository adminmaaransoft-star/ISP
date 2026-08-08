//go:build integration

// Integration tests for the billing module.
//
// Covers INT-BIL-001 .. INT-BIL-005 from the Integration Tests tracker sheet.
// The wallet ledger is modelled by an in-memory store that enforces the same
// invariants as migrations/007_create_wallet_ledgers.sql: a unique index on
// non-null transaction_token, and all-or-nothing posting of both legs.
//
// INT-BIL-006 (the DB CHECK that blocks CGST+SGST and IGST on one invoice) is a
// schema-level assertion and lives in scripts/int_bil_006_gst_constraint.sh.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/billing -Tags integration
package billing_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// ── In-memory wallet ledger ─────────────────────────────────────────────────

// itLedger mimics the wallet_ledgers table plus subscribers.wallet_balance.
type itLedger struct {
	mu        sync.Mutex
	rows      []billing.WalletEntry
	balances  map[int]decimal.Decimal
	txByToken map[string]billing.Transaction
	nextID    int
	// failPosting simulates a mid-posting failure.
	failPosting bool
}

func newITLedger() *itLedger {
	return &itLedger{
		balances:  map[int]decimal.Decimal{},
		txByToken: map[string]billing.Transaction{},
		nextID:    1,
	}
}

func (l *itLedger) GetSubscriberBalance(_ context.Context, subscriberID int) (decimal.Decimal, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bal, ok := l.balances[subscriberID]
	if !ok {
		return decimal.Zero, nil
	}
	return bal, nil
}

func (l *itLedger) GetTransactionByToken(_ context.Context, token string) (*billing.Transaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	tx, ok := l.txByToken[token]
	if !ok {
		return nil, nil
	}
	return &tx, nil
}

// RecordRecharge writes both legs and the balance atomically: on the unique
// token violation nothing is persisted, matching a rolled-back DB transaction.
func (l *itLedger) RecordRecharge(_ context.Context, p billing.RechargePosting) (*billing.Transaction, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.failPosting {
		return nil, errors.New("simulated posting failure")
	}

	// Enforce idx_wallet_token: unique on transaction_token where not null.
	token := ""
	if p.Credit.TransactionToken != nil {
		token = *p.Credit.TransactionToken
		if _, exists := l.txByToken[token]; exists {
			return nil, errors.New("duplicate key value violates unique constraint \"idx_wallet_token\"")
		}
	}

	l.rows = append(l.rows, p.Debit, p.Credit)
	l.balances[p.SubscriberID] = p.NewBalance
	id := l.nextID
	l.nextID++

	tx := billing.Transaction{
		ID:               id,
		SubscriberID:     p.Credit.SubscriberID,
		EntryType:        p.Credit.EntryType,
		Amount:           p.Credit.Amount,
		BalanceAfter:     p.NewBalance,
		TransactionToken: token,
		Description:      p.Credit.Description,
	}
	if token != "" {
		l.txByToken[token] = tx
	}
	return &tx, nil
}

func (l *itLedger) rowsFor(subscriberID int) []billing.WalletEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []billing.WalletEntry
	for _, row := range l.rows {
		if row.SubscriberID == subscriberID {
			out = append(out, row)
		}
	}
	return out
}

// ── INT-BIL-001 ─────────────────────────────────────────────────────────────

// TestWalletRecharge_DoubleLedger verifies a recharge posts both ledger legs and
// updates the subscriber balance.
//
// INT-BIL-001 | FR-BIL-003
func TestWalletRecharge_DoubleLedger(t *testing.T) {
	ledger := newITLedger()
	svc := billing.NewWalletService(ledger)

	amount := decimal.RequireFromString("799.00")
	tx, err := svc.Recharge(context.Background(), billing.RechargeRequest{
		SubscriberID:     1,
		Amount:           amount,
		TransactionToken: "pay_LxRr9wZ1",
		Description:      "recharge via razorpay",
	})
	if err != nil {
		t.Fatalf("Recharge: %v", err)
	}

	rows := ledger.rowsFor(1)
	if len(rows) != 2 {
		t.Fatalf("wallet_ledgers: want 2 rows (debit + credit), got %d", len(rows))
	}

	var debit, credit *billing.WalletEntry
	for i := range rows {
		switch rows[i].EntryType {
		case "debit":
			debit = &rows[i]
		case "credit":
			credit = &rows[i]
		}
	}
	if debit == nil || credit == nil {
		t.Fatalf("want one debit and one credit leg, got %+v", rows)
	}
	if !debit.Amount.Equal(credit.Amount) {
		t.Errorf("legs must balance: debit %s != credit %s", debit.Amount, credit.Amount)
	}
	if credit.Account != billing.AccountSubscriberWallet {
		t.Errorf("credit account: want %s, got %s", billing.AccountSubscriberWallet, credit.Account)
	}
	if debit.Account != billing.AccountGatewayClearing {
		t.Errorf("debit account: want %s, got %s", billing.AccountGatewayClearing, debit.Account)
	}
	// Only the credit leg may carry the token — idx_wallet_token is unique.
	if debit.TransactionToken != nil {
		t.Errorf("debit leg must not carry the idempotency token, got %q", *debit.TransactionToken)
	}

	balance, _ := ledger.GetSubscriberBalance(context.Background(), 1)
	if !balance.Equal(amount) {
		t.Errorf("wallet_balance: want %s, got %s", amount, balance)
	}
	if !tx.BalanceAfter.Equal(amount) {
		t.Errorf("tx.BalanceAfter: want %s, got %s", amount, tx.BalanceAfter)
	}
}

// TestWalletRecharge_FailedPostingLeavesNoPartialState verifies a failure while
// posting leaves neither ledger rows nor a changed balance behind.
//
// INT-BIL-001 (supporting) | FR-BIL-003
func TestWalletRecharge_FailedPostingLeavesNoPartialState(t *testing.T) {
	ledger := newITLedger()
	ledger.failPosting = true
	svc := billing.NewWalletService(ledger)

	_, err := svc.Recharge(context.Background(), billing.RechargeRequest{
		SubscriberID:     7,
		Amount:           decimal.RequireFromString("500.00"),
		TransactionToken: "pay_fails",
	})
	if err == nil {
		t.Fatal("expected error when posting fails")
	}
	if rows := ledger.rowsFor(7); len(rows) != 0 {
		t.Errorf("want no ledger rows after failed posting, got %d", len(rows))
	}
	balance, _ := ledger.GetSubscriberBalance(context.Background(), 7)
	if !balance.IsZero() {
		t.Errorf("want unchanged zero balance after failed posting, got %s", balance)
	}
}

// ── INT-BIL-002 ─────────────────────────────────────────────────────────────

// TestWalletRecharge_Idempotent verifies a replayed transaction_token returns
// the original transaction and does not credit the wallet twice.
//
// INT-BIL-002 | FR-BIL-005
func TestWalletRecharge_Idempotent(t *testing.T) {
	ledger := newITLedger()
	svc := billing.NewWalletService(ledger)
	ctx := context.Background()

	req := billing.RechargeRequest{
		SubscriberID:     2,
		Amount:           decimal.RequireFromString("799.00"),
		TransactionToken: "pay_duplicate_001",
		Description:      "recharge via razorpay",
	}

	tx1, err := svc.Recharge(ctx, req)
	if err != nil {
		t.Fatalf("first Recharge: %v", err)
	}
	tx2, err := svc.Recharge(ctx, req)
	if err != nil {
		t.Fatalf("replayed Recharge must succeed, got: %v", err)
	}

	if tx1.ID != tx2.ID {
		t.Errorf("replay must return the original transaction: tx1.ID=%d tx2.ID=%d", tx1.ID, tx2.ID)
	}
	balance, _ := ledger.GetSubscriberBalance(ctx, 2)
	if !balance.Equal(req.Amount) {
		t.Errorf("balance after replay: want a single recharge of %s, got %s", req.Amount, balance)
	}
	if rows := ledger.rowsFor(2); len(rows) != 2 {
		t.Errorf("replay must not add ledger rows: want 2, got %d", len(rows))
	}
}

// TestWalletRecharge_DistinctTokensBothApply guards against the idempotency
// check being too broad and swallowing genuine second payments.
//
// INT-BIL-002 (supporting) | FR-BIL-005
func TestWalletRecharge_DistinctTokensBothApply(t *testing.T) {
	ledger := newITLedger()
	svc := billing.NewWalletService(ledger)
	ctx := context.Background()

	for _, token := range []string{"pay_A", "pay_B"} {
		if _, err := svc.Recharge(ctx, billing.RechargeRequest{
			SubscriberID:     3,
			Amount:           decimal.RequireFromString("100.00"),
			TransactionToken: token,
		}); err != nil {
			t.Fatalf("Recharge %s: %v", token, err)
		}
	}

	balance, _ := ledger.GetSubscriberBalance(ctx, 3)
	if want := decimal.RequireFromString("200.00"); !balance.Equal(want) {
		t.Errorf("balance after two distinct payments: want %s, got %s", want, balance)
	}
	if rows := ledger.rowsFor(3); len(rows) != 4 {
		t.Errorf("want 4 ledger rows for two recharges, got %d", len(rows))
	}
}

// ── INT-BIL-003 / INT-BIL-004 ───────────────────────────────────────────────

// itRazorpayWebhook is a minimal endpoint wired the way the API service mounts
// the real one: validate the HMAC over the raw body, then credit the wallet.
func itRazorpayWebhook(svc *billing.WalletService, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := new(bytes.Buffer)
		if _, err := body.ReadFrom(r.Body); err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}
		if err := billing.ValidateRazorpaySignature(body.Bytes(), r.Header.Get("X-Razorpay-Signature"), secret); err != nil {
			http.Error(w, "invalid signature", http.StatusBadRequest)
			return
		}

		var payload struct {
			Payload struct {
				Payment struct {
					Entity struct {
						ID           string `json:"id"`
						Amount       int64  `json:"amount"` // paise
						SubscriberID int    `json:"subscriber_id"`
					} `json:"entity"`
				} `json:"payment"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(body.Bytes(), &payload); err != nil {
			http.Error(w, "invalid payload", http.StatusBadRequest)
			return
		}

		entity := payload.Payload.Payment.Entity
		amount := decimal.NewFromInt(entity.Amount).Div(decimal.NewFromInt(100))
		if _, err := svc.Recharge(r.Context(), billing.RechargeRequest{
			SubscriberID:     entity.SubscriberID,
			Amount:           amount,
			TransactionToken: entity.ID,
			Description:      "recharge via razorpay webhook",
		}); err != nil {
			http.Error(w, "recharge failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func itWebhookBody(subscriberID int, paymentID string, paise int64) []byte {
	b, _ := json.Marshal(map[string]any{
		"event": "payment.captured",
		"payload": map[string]any{
			"payment": map[string]any{
				"entity": map[string]any{
					"id":            paymentID,
					"amount":        paise,
					"subscriber_id": subscriberID,
				},
			},
		},
	})
	return b
}

// TestWebhookHMAC_ValidSignatureAccepted verifies a correctly signed Razorpay
// callback is accepted and credits the wallet.
//
// INT-BIL-003 | FR-SEC-004
func TestWebhookHMAC_ValidSignatureAccepted(t *testing.T) {
	const secret = "razorpay_webhook_secret"
	ledger := newITLedger()
	handler := itRazorpayWebhook(billing.NewWalletService(ledger), secret)

	body := itWebhookBody(4, "pay_valid_hmac", 79900)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader(body))
	req.Header.Set("X-Razorpay-Signature", hmacSHA256(body, secret))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for valid signature, got %d — %s", rec.Code, rec.Body.String())
	}
	rows := ledger.rowsFor(4)
	if len(rows) != 2 {
		t.Fatalf("want 2 ledger rows after credited webhook, got %d", len(rows))
	}
	balance, _ := ledger.GetSubscriberBalance(context.Background(), 4)
	if want := decimal.RequireFromString("799.00"); !balance.Equal(want) {
		t.Errorf("balance: want %s, got %s", want, balance)
	}
}

// TestWebhookHMAC_InvalidSignatureRejected verifies a tampered callback is
// rejected with 400 and leaves the wallet untouched.
//
// INT-BIL-004 | FR-SEC-004
func TestWebhookHMAC_InvalidSignatureRejected(t *testing.T) {
	const secret = "razorpay_webhook_secret"
	ledger := newITLedger()
	handler := itRazorpayWebhook(billing.NewWalletService(ledger), secret)

	body := itWebhookBody(5, "pay_bad_hmac", 79900)

	cases := []struct {
		name      string
		signature string
	}{
		{"wrong signature", hmacSHA256(body, "attacker_secret")},
		{"empty signature", ""},
		{"truncated signature", hmacSHA256(body, secret)[:32]},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader(body))
			req.Header.Set("X-Razorpay-Signature", c.signature)
			rec := httptest.NewRecorder()

			handler(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d", rec.Code)
			}
		})
	}

	if rows := ledger.rowsFor(5); len(rows) != 0 {
		t.Errorf("rejected webhook must create no ledger entry, got %d rows", len(rows))
	}
	balance, _ := ledger.GetSubscriberBalance(context.Background(), 5)
	if !balance.IsZero() {
		t.Errorf("balance must be unchanged after rejected webhook, got %s", balance)
	}
}

// TestWebhookHMAC_TamperedBodyRejected verifies that altering the body after
// signing invalidates the signature.
//
// INT-BIL-004 (supporting) | FR-SEC-004
func TestWebhookHMAC_TamperedBodyRejected(t *testing.T) {
	const secret = "razorpay_webhook_secret"
	ledger := newITLedger()
	handler := itRazorpayWebhook(billing.NewWalletService(ledger), secret)

	original := itWebhookBody(6, "pay_tamper", 100)
	signature := hmacSHA256(original, secret)
	inflated := itWebhookBody(6, "pay_tamper", 9999900) // attacker raises the amount

	req := httptest.NewRequest(http.MethodPost, "/webhooks/razorpay", bytes.NewReader(inflated))
	req.Header.Set("X-Razorpay-Signature", signature)
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for body tampered after signing, got %d", rec.Code)
	}
	if rows := ledger.rowsFor(6); len(rows) != 0 {
		t.Errorf("tampered webhook must create no ledger entry, got %d rows", len(rows))
	}
}

// ── INT-BIL-005 ─────────────────────────────────────────────────────────────

// TestCalculateGstInvoice_TN verifies the exact CGST/SGST/IGST split and total
// for a Tamil Nadu intrastate invoice on a ₹799.00 base.
//
// INT-BIL-005 | FR-BIL-001
func TestCalculateGstInvoice_TN(t *testing.T) {
	rate := billing.GstRate{
		ID:       1,
		CgstRate: decimal.RequireFromString("9.00"),
		SgstRate: decimal.RequireFromString("9.00"),
		IgstRate: decimal.RequireFromString("18.00"),
	}

	inv := billing.CalculateGstInvoice(decimal.RequireFromString("799.00"), "TN", rate)

	want := map[string]decimal.Decimal{
		"CGST":  decimal.RequireFromString("71.91"),
		"SGST":  decimal.RequireFromString("71.91"),
		"IGST":  decimal.Zero,
		"Total": decimal.RequireFromString("942.82"),
	}
	got := map[string]decimal.Decimal{
		"CGST":  inv.CgstAmount,
		"SGST":  inv.SgstAmount,
		"IGST":  inv.IgstAmount,
		"Total": inv.TotalAmount,
	}
	for field, expected := range want {
		if !got[field].Equal(expected) {
			t.Errorf("%s: want %s, got %s", field, expected, got[field])
		}
	}

	// Intrastate invoices must never carry IGST alongside CGST/SGST — the DB
	// enforces this too (INT-BIL-006).
	if !inv.IgstAmount.IsZero() && !inv.CgstAmount.IsZero() {
		t.Error("invoice carries both IGST and CGST, violating the GST split rule")
	}
}
