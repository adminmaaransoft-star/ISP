package billing

import "time"

// SetScannerClock lets the external billing_test package drive the dunning
// scanner from a fixed instant. Every rule the scanner applies is a comparison
// against time.Now, so without this the boundary cases could only be tested by
// computing expiries relative to the real clock — which makes the arithmetic
// in each test the thing under test rather than the ladder.
//
// This file is compiled only under `go test`, so the hook does not widen the
// package's real API.
func SetScannerClock(s *DunningScanner, now func() time.Time) {
	s.now = now
}

// SetRecurringBillingScannerClock is the auto-renewal scanner's equivalent of
// SetScannerClock, for the same reason: max(now, currentExpiry) needs a fixed
// instant to make the boundary deterministic.
func SetRecurringBillingScannerClock(s *RecurringBillingScanner, now func() time.Time) {
	s.now = now
}
