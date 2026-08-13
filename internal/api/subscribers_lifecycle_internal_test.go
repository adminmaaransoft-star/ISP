package api

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// White-box (package api, not api_test): computePlanChangeExpiry is the
// proration formula MDS §4.14 specifies, and is deliberately unexported —
// verified directly here rather than only indirectly through an HTTP-level
// test, since a proration bug that happened to land on the right day for one
// scenario could otherwise hide behind a handler test that never varies the
// inputs enough to expose it.

func decP(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestComputePlanChangeExpiry(t *testing.T) {
	now := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		info *PlanChangeInfo
		want time.Time
	}{
		{
			name: "no remaining validity: new plan's own validity only, zero bonus days",
			info: &PlanChangeInfo{
				CurrentExpiry: nil, // never renewed / already lapsed
				OldPrice:      decP("500.00"), OldValidityDays: 30,
				NewPrice: decP("1000.00"), NewValidityDays: 30,
			},
			want: now.AddDate(0, 0, 30),
		},
		{
			name: "upgrade with remaining value: bonus days on top of new validity",
			info: &PlanChangeInfo{
				CurrentExpiry: timePtr(now.Add(10 * 24 * time.Hour)), // 10 days remaining
				OldPrice:      decP("300.00"), OldValidityDays: 30,   // 10/day
				NewPrice: decP("600.00"), NewValidityDays: 30, // 20/day
			},
			// credit = 10 days * 10/day = 100; bonus = 100/20 = 5 days.
			want: now.AddDate(0, 0, 35),
		},
		{
			name: "downgrade with remaining value: bonus days computed at the new, cheaper rate",
			info: &PlanChangeInfo{
				CurrentExpiry: timePtr(now.Add(10 * 24 * time.Hour)), // 10 days remaining
				OldPrice:      decP("600.00"), OldValidityDays: 30,   // 20/day
				NewPrice: decP("300.00"), NewValidityDays: 30, // 10/day
			},
			// credit = 10 * 20 = 200; bonus = 200/10 = 20 days.
			want: now.AddDate(0, 0, 50),
		},
		{
			name: "expired old plan (CurrentExpiry in the past): no negative credit",
			info: &PlanChangeInfo{
				CurrentExpiry: timePtr(now.Add(-5 * 24 * time.Hour)), // already lapsed
				OldPrice:      decP("500.00"), OldValidityDays: 30,
				NewPrice: decP("500.00"), NewValidityDays: 30,
			},
			want: now.AddDate(0, 0, 30),
		},
		{
			name: "fractional-day remainder floors rather than rounds up",
			info: &PlanChangeInfo{
				CurrentExpiry: timePtr(now.Add(9 * 24 * time.Hour)), // 9 days remaining
				OldPrice:      decP("300.00"), OldValidityDays: 30,  // 10/day
				NewPrice: decP("600.00"), NewValidityDays: 30, // 20/day
			},
			// credit = 9*10 = 90; bonus = 90/20 = 4.5 -> floors to 4.
			want: now.AddDate(0, 0, 34),
		},
		{
			name: "free new plan (price zero): no divide-by-zero, zero bonus days",
			info: &PlanChangeInfo{
				CurrentExpiry: timePtr(now.Add(10 * 24 * time.Hour)),
				OldPrice:      decP("500.00"), OldValidityDays: 30,
				NewPrice: decimal.Zero, NewValidityDays: 30,
			},
			want: now.AddDate(0, 0, 30),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computePlanChangeExpiry(tc.info, now)
			if !got.Equal(tc.want) {
				t.Errorf("computePlanChangeExpiry: want %v, got %v", tc.want, got)
			}
		})
	}
}

func timePtr(t time.Time) *time.Time { return &t }
