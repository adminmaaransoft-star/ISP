package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakePlanExpiryStore struct {
	validityDays  int
	currentExpiry *time.Time
	setExpiry     time.Time
	setCalled     bool
	getErr        error
	setErr        error
}

func (f *fakePlanExpiryStore) GetPlanRenewalInfo(_ context.Context, _ int) (int, *time.Time, error) {
	if f.getErr != nil {
		return 0, nil, f.getErr
	}
	return f.validityDays, f.currentExpiry, nil
}

func (f *fakePlanExpiryStore) SetPlanExpiry(_ context.Context, _ int, expiry time.Time) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setExpiry = expiry
	f.setCalled = true
	return nil
}

func timePtr(t time.Time) *time.Time { return &t }

// TestExtendPlanExpiry_DateMath covers the fix for the renewal callback
// silently crediting the wallet without ever extending plan_expiry: the rule
// is max(now, currentExpiry) + validityDays, never just "now + validityDays".
func TestExtendPlanExpiry_DateMath(t *testing.T) {
	fixedNow := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return fixedNow }

	tests := []struct {
		name          string
		validityDays  int
		currentExpiry *time.Time
		wantExpiry    time.Time
	}{
		{
			name:          "future expiry extends from the current expiry, not from now",
			validityDays:  30,
			currentExpiry: timePtr(fixedNow.AddDate(0, 0, 10)), // 10 paid days still remaining
			wantExpiry:    fixedNow.AddDate(0, 0, 10).AddDate(0, 0, 30),
		},
		{
			name:          "past (lapsed) expiry extends from now, not from the stale date",
			validityDays:  30,
			currentExpiry: timePtr(fixedNow.AddDate(0, 0, -5)), // lapsed 5 days ago
			wantExpiry:    fixedNow.AddDate(0, 0, 30),
		},
		{
			name:          "nil expiry (never set) extends from now",
			validityDays:  30,
			currentExpiry: nil,
			wantExpiry:    fixedNow.AddDate(0, 0, 30),
		},
		{
			name:          "expiry exactly equal to now extends from now, not doubled",
			validityDays:  15,
			currentExpiry: timePtr(fixedNow),
			wantExpiry:    fixedNow.AddDate(0, 0, 15),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakePlanExpiryStore{validityDays: tt.validityDays, currentExpiry: tt.currentExpiry}
			if err := extendPlanExpiry(context.Background(), store, 1, now); err != nil {
				t.Fatalf("extendPlanExpiry: %v", err)
			}
			if !store.setCalled {
				t.Fatal("expected SetPlanExpiry to be called")
			}
			if !store.setExpiry.Equal(tt.wantExpiry) {
				t.Errorf("expiry = %v, want %v", store.setExpiry, tt.wantExpiry)
			}
		})
	}
}

func TestExtendPlanExpiry_PropagatesGetError(t *testing.T) {
	store := &fakePlanExpiryStore{getErr: errors.New("db down")}
	if err := extendPlanExpiry(context.Background(), store, 1, time.Now); err == nil {
		t.Fatal("expected an error when GetPlanRenewalInfo fails")
	}
	if store.setCalled {
		t.Error("SetPlanExpiry must not be called if GetPlanRenewalInfo failed")
	}
}

func TestExtendPlanExpiry_PropagatesSetError(t *testing.T) {
	store := &fakePlanExpiryStore{validityDays: 30, setErr: errors.New("db down")}
	if err := extendPlanExpiry(context.Background(), store, 1, time.Now); err == nil {
		t.Fatal("expected an error when SetPlanExpiry fails")
	}
}
