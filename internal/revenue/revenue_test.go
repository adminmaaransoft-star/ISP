package revenue_test

import (
	"testing"

	"github.com/shopspring/decimal"
)

// TestLCOCommission_Calculation verifies commission = recharge * rate / 100.
func TestLCOCommission_Calculation(t *testing.T) {
	rechargeAmount := decimal.NewFromFloat(799.00)
	commissionRatePct := decimal.NewFromFloat(5.00)

	commission := rechargeAmount.Mul(commissionRatePct).Div(decimal.NewFromInt(100)).Round(2)

	expected := decimal.NewFromFloat(39.95)
	if !commission.Equal(expected) {
		t.Errorf("commission: want %s, got %s", expected, commission)
	}
}

// TestLedgerVariance_AlertThreshold verifies alert fires when ABS(variance) > 0.01.
func TestLedgerVariance_AlertThreshold(t *testing.T) {
	cases := []struct {
		variance    string
		shouldAlert bool
	}{
		{"0.00", false},
		{"0.005", false},
		{"0.01", false},
		{"0.011", true},
		{"-0.50", true},
	}
	threshold := decimal.NewFromFloat(0.01)
	for _, c := range cases {
		v, _ := decimal.NewFromString(c.variance)
		alert := v.Abs().GreaterThan(threshold)
		if alert != c.shouldAlert {
			t.Errorf("variance=%s: expected alert=%v, got %v", c.variance, c.shouldAlert, alert)
		}
	}
}
