package billing_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

func hmacSHA256(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestCalculateGstInvoice_Intrastate verifies CGST+SGST split for TN state.
func TestCalculateGstInvoice_Intrastate(t *testing.T) {
	rate := billing.GstRate{ID: 1,
		CgstRate: decimal.NewFromFloat(9.0),
		SgstRate: decimal.NewFromFloat(9.0),
		IgstRate: decimal.NewFromFloat(18.0),
	}
	inv := billing.CalculateGstInvoice(decimal.NewFromFloat(799.00), "TN", rate)

	// 799 * 9% = 71.91 (each)
	if !inv.CgstAmount.Equal(decimal.NewFromFloat(71.91)) {
		t.Errorf("CGST: want 71.91, got %s", inv.CgstAmount)
	}
	if !inv.SgstAmount.Equal(decimal.NewFromFloat(71.91)) {
		t.Errorf("SGST: want 71.91, got %s", inv.SgstAmount)
	}
	if !inv.IgstAmount.IsZero() {
		t.Errorf("IGST must be 0 for intrastate, got %s", inv.IgstAmount)
	}
	expectedTotal := decimal.NewFromFloat(799.00 + 71.91 + 71.91)
	if !inv.TotalAmount.Equal(expectedTotal) {
		t.Errorf("Total: want %s, got %s", expectedTotal, inv.TotalAmount)
	}
}

// TestCalculateGstInvoice_Interstate verifies IGST only for non-TN state.
func TestCalculateGstInvoice_Interstate(t *testing.T) {
	rate := billing.GstRate{ID: 1,
		CgstRate: decimal.NewFromFloat(9.0),
		SgstRate: decimal.NewFromFloat(9.0),
		IgstRate: decimal.NewFromFloat(18.0),
	}
	inv := billing.CalculateGstInvoice(decimal.NewFromFloat(799.00), "KA", rate)

	if !inv.CgstAmount.IsZero() {
		t.Errorf("CGST must be 0 for interstate, got %s", inv.CgstAmount)
	}
	if !inv.SgstAmount.IsZero() {
		t.Errorf("SGST must be 0 for interstate, got %s", inv.SgstAmount)
	}
	// 799 * 18% = 143.82
	if !inv.IgstAmount.Equal(decimal.NewFromFloat(143.82)) {
		t.Errorf("IGST: want 143.82, got %s", inv.IgstAmount)
	}
}

// TestValidateRazorpaySignature_InvalidSig verifies that a wrong signature is rejected.
func TestValidateRazorpaySignature_InvalidSig(t *testing.T) {
	payload := []byte("test_payload")
	secret := "test_secret"
	err := billing.ValidateRazorpaySignature(payload, "wrong_sig_value_here", secret)
	if err == nil {
		t.Error("expected error for wrong signature, got nil")
	}
}

// TestValidateRazorpaySignature_ValidSig verifies that the correct HMAC-SHA256 is accepted.
func TestValidateRazorpaySignature_ValidSig(t *testing.T) {
	payload := []byte("pay_body")
	secret := "sec"
	mac := hmacSHA256(payload, secret)
	err := billing.ValidateRazorpaySignature(payload, mac, secret)
	if err != nil {
		t.Errorf("expected nil error for valid signature, got %v", err)
	}
}

// dunningSetCall records one SetSubscriberDunningState invocation.
type dunningSetCall struct {
	state  billing.DunningState
	status string
}

// fakeDunningQuerier is a package-level test double for
// billing.DunningQuerier — no real DB needed to exercise the state machine
// logic in TransitionDunning itself.
type fakeDunningQuerier struct {
	currentState billing.DunningState
	getErr       error
	setErr       error
	setCalls     []dunningSetCall
}

func (f *fakeDunningQuerier) GetSubscriberDunningState(_ context.Context, _ int) (billing.DunningState, time.Time, error) {
	if f.getErr != nil {
		return "", time.Time{}, f.getErr
	}
	return f.currentState, time.Time{}, nil
}

func (f *fakeDunningQuerier) SetSubscriberDunningState(_ context.Context, _ int, state billing.DunningState, status string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, dunningSetCall{state: state, status: status})
	return nil
}

// TestDunningTransition_ValidEdge verifies a permitted state machine
// transition both succeeds and persists the correct target state.
func TestDunningTransition_ValidEdge(t *testing.T) {
	fake := &fakeDunningQuerier{currentState: billing.DunningActive}

	if err := billing.TransitionDunning(context.Background(), fake, 1, billing.DunningRemind7d); err != nil {
		t.Fatalf("TransitionDunning: %v", err)
	}
	if len(fake.setCalls) != 1 {
		t.Fatalf("want 1 SetSubscriberDunningState call, got %d", len(fake.setCalls))
	}
	if fake.setCalls[0].state != billing.DunningRemind7d {
		t.Errorf("state: want %s, got %s", billing.DunningRemind7d, fake.setCalls[0].state)
	}
}

// TestDunningTransition_InvalidEdgeRejected verifies a transition with no
// matching edge in validTransitions is rejected and never persisted —
// active cannot jump straight to hard_suspended, skipping every reminder
// and grace stage.
func TestDunningTransition_InvalidEdgeRejected(t *testing.T) {
	fake := &fakeDunningQuerier{currentState: billing.DunningActive}

	err := billing.TransitionDunning(context.Background(), fake, 1, billing.DunningHardSuspended)
	if err == nil {
		t.Fatal("expected an error for an invalid transition, got nil")
	}
	if len(fake.setCalls) != 0 {
		t.Error("no state change should be persisted for an invalid transition")
	}
}

func TestDunningTransition_GetStateErrorPropagates(t *testing.T) {
	fake := &fakeDunningQuerier{getErr: errors.New("db down")}
	if err := billing.TransitionDunning(context.Background(), fake, 1, billing.DunningRemind7d); err == nil {
		t.Fatal("expected the GetSubscriberDunningState error to propagate")
	}
}

func TestDunningTransition_SetStateErrorPropagates(t *testing.T) {
	fake := &fakeDunningQuerier{currentState: billing.DunningActive, setErr: errors.New("db down")}
	if err := billing.TransitionDunning(context.Background(), fake, 1, billing.DunningRemind7d); err == nil {
		t.Fatal("expected the SetSubscriberDunningState error to propagate")
	}
}

// TestDunningToSubscriberStatus exercises every branch of the unexported
// dunningToSubscriberStatus mapping — indirectly, through the status
// TransitionDunning passes to SetSubscriberDunningState, since the mapping
// function itself is unexported and this package's tests live in
// billing_test (external), not billing. Covers every edge in
// validTransitions, so every dunning state this codebase can actually reach
// is exercised, not just a hand-picked few.
func TestDunningToSubscriberStatus(t *testing.T) {
	cases := []struct {
		from, to   billing.DunningState
		wantStatus string
	}{
		{billing.DunningActive, billing.DunningRemind7d, "active"},
		{billing.DunningRemind7d, billing.DunningRemind3d, "active"},
		{billing.DunningRemind3d, billing.DunningRemind1d, "active"},
		{billing.DunningRemind1d, billing.DunningGracePeriod, "grace_period"},
		{billing.DunningGracePeriod, billing.DunningSoftSuspended, "soft_suspended"},
		{billing.DunningSoftSuspended, billing.DunningHardSuspended, "hard_suspended"},
		{billing.DunningGracePeriod, billing.DunningActive, "active"},
		{billing.DunningSoftSuspended, billing.DunningActive, "active"},
		{billing.DunningHardSuspended, billing.DunningActive, "active"},
		// Renewing while still in a reminder stage (MDS §4.14's auto-renewal
		// restore path, and any plan renewal generally) must also be able to
		// walk back to active — these three edges were missing until fixed
		// alongside the auto-renewal scanner.
		{billing.DunningRemind7d, billing.DunningActive, "active"},
		{billing.DunningRemind3d, billing.DunningActive, "active"},
		{billing.DunningRemind1d, billing.DunningActive, "active"},
	}
	for _, tc := range cases {
		t.Run(string(tc.from)+"->"+string(tc.to), func(t *testing.T) {
			fake := &fakeDunningQuerier{currentState: tc.from}
			if err := billing.TransitionDunning(context.Background(), fake, 1, tc.to); err != nil {
				t.Fatalf("TransitionDunning(%s -> %s): %v", tc.from, tc.to, err)
			}
			if len(fake.setCalls) != 1 {
				t.Fatalf("want 1 SetSubscriberDunningState call, got %d", len(fake.setCalls))
			}
			if fake.setCalls[0].status != tc.wantStatus {
				t.Errorf("status: want %s, got %s", tc.wantStatus, fake.setCalls[0].status)
			}
		})
	}
}
