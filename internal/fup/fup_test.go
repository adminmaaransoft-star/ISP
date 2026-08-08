package fup_test

import (
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/fup"
)

// TestUsagePct_BelowThreshold verifies percentage below 80%.
func TestUsagePct_BelowThreshold(t *testing.T) {
	pct := fup.UsagePct(1_500_000_000, 3_543_348_019_200)
	if pct >= fup.FUPWarningPct {
		t.Errorf("expected pct < %d for 1.5GB / 3.3TB, got %d", fup.FUPWarningPct, pct)
	}
}

// TestUsagePct_AtWarning verifies that 80% usage triggers warning threshold.
func TestUsagePct_AtWarning(t *testing.T) {
	threshold := int64(3_543_348_019_200) // 3.3TB
	used := int64(float64(threshold) * 0.80)
	pct := fup.UsagePct(used, threshold)
	if pct < fup.FUPWarningPct {
		t.Errorf("expected pct >= %d at 80%% usage, got %d", fup.FUPWarningPct, pct)
	}
}

// TestUsagePct_Unlimited verifies that threshold=0 returns 0% (unlimited plan).
func TestUsagePct_Unlimited(t *testing.T) {
	pct := fup.UsagePct(99_999_999_999, 0)
	if pct != 0 {
		t.Errorf("expected 0 for unlimited plan (threshold=0), got %d", pct)
	}
}

// TestUsagePct_AboveThreshold verifies breach detection.
func TestUsagePct_AboveThreshold(t *testing.T) {
	threshold := int64(1_771_674_009_600) // 1.65TB
	used := threshold + 1
	pct := fup.UsagePct(used, threshold)
	if pct < 100 {
		t.Errorf("expected pct >= 100 above threshold, got %d", pct)
	}
}
