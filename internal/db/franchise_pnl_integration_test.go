//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/revenue"
)

// Franchise P&L persistence tests — FR-FRN-003..006 | MDS §4.10.
//
// internal/api/franchises_integration_test.go covers the HTTP layer against
// a stub, which never touches this SQL. These tests exercise the real query
// — in particular franchisePnLSQL's two separate LEFT JOIN subqueries, which
// exist specifically to avoid a row-multiplication bug: joining subscribers
// and lco_ledger to a franchise in one pass multiplies rows (3 subscribers ×
// 4 recharges = 12), silently inflating both counts. A test with only one
// subscriber or one recharge per franchise could not tell the correct query
// apart from the broken one — these seed multiple of both.

func TestFR_FRN_003_GetFranchisePnL_AggregatesWithoutRowMultiplication(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 1, "Chennai North", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")

	// Three subscribers under the franchise: the subscriber count must land
	// on 3, not on 3×(recharge count) from a naive join.
	franchiseID := 1
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "lco1_a", FranchiseID: &franchiseID})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "lco1_b", FranchiseID: &franchiseID})
	seedSubscriber(ctx, t, pool, 3, seedOpts{Username: "lco1_c", FranchiseID: &franchiseID})

	// Four recharges, deliberately not matching the subscriber count, so a
	// row-multiplication bug (3 subscribers × 4 recharges = 12) is
	// impossible to mistake for the correct answer (3 and 4).
	for i, amount := range []string{"500.00", "500.00", "300.00", "700.00"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO lco_ledger (franchise_id, subscriber_id, recharge_amount, commission_amount, transaction_ref)
			VALUES ($1, $2, $3::numeric, ($3::numeric * 0.10), $4)`,
			franchiseID, (i%3)+1, amount, "txn-"+string(rune('a'+i))); err != nil {
			t.Fatalf("seed recharge %d: %v", i, err)
		}
	}

	pnl, err := database.Revenue().GetFranchisePnL(ctx, 1, nil, nil)
	if err != nil {
		t.Fatalf("GetFranchisePnL: %v", err)
	}
	if pnl == nil {
		t.Fatal("GetFranchisePnL returned nil for a franchise that exists")
	}

	if pnl.SubscriberCount != 3 {
		t.Errorf("subscriber_count = %d, want 3 — 12 would mean the join multiplied rows", pnl.SubscriberCount)
	}
	if pnl.RechargeCount != 4 {
		t.Errorf("recharge_count = %d, want 4 — 12 would mean the join multiplied rows", pnl.RechargeCount)
	}
	if pnl.TotalRecharges != "2000.00" {
		t.Errorf("total_recharges = %q, want 2000.00 (500+500+300+700)", pnl.TotalRecharges)
	}
	if pnl.CommissionEarned != "200.00" {
		t.Errorf("commission_earned = %q, want 200.00 (10%% of 2000.00)", pnl.CommissionEarned)
	}
	if pnl.NetToISP != "1800.00" {
		t.Errorf("net_to_isp = %q, want 1800.00 (2000.00 - 200.00)", pnl.NetToISP)
	}
}

func TestFR_FRN_003_GetFranchisePnL_UnknownFranchise_ReturnsNilNotError(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	pnl, err := database.Revenue().GetFranchisePnL(ctx, 999999, nil, nil)
	if err != nil {
		t.Fatalf("GetFranchisePnL for an unknown franchise should not error, got: %v", err)
	}
	if pnl != nil {
		t.Errorf("want nil for an unknown franchise, got %+v", pnl)
	}
}

// TestFR_FRN_003_GetFranchisePnL_OnboardedButIdle_ReportsZeroesNotAbsence
// verifies a partner with no subscribers and no recharges yet is a real,
// present row with zero values — not indistinguishable from "does not exist".
func TestFR_FRN_003_GetFranchisePnL_OnboardedButIdle_ReportsZeroesNotAbsence(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 2, "New Partner", "15.00", "active")

	pnl, err := database.Revenue().GetFranchisePnL(ctx, 2, nil, nil)
	if err != nil {
		t.Fatalf("GetFranchisePnL: %v", err)
	}
	if pnl == nil {
		t.Fatal("an onboarded partner with no activity must still return a row, not nil")
	}
	if pnl.SubscriberCount != 0 || pnl.RechargeCount != 0 {
		t.Errorf("idle partner: subscriber_count=%d recharge_count=%d, want 0 and 0", pnl.SubscriberCount, pnl.RechargeCount)
	}
	if pnl.TotalRecharges != "0.00" || pnl.CommissionEarned != "0.00" {
		t.Errorf("idle partner totals: recharges=%q commission=%q, want 0.00 and 0.00",
			pnl.TotalRecharges, pnl.CommissionEarned)
	}
}

// TestFR_FRN_003_GetFranchisePnL_DateWindow_ExcludesRechargesOutsideIt
// verifies the ?from=/?to= filter actually reaches the SQL rather than being
// parsed and silently dropped.
func TestFR_FRN_003_GetFranchisePnL_DateWindow_ExcludesRechargesOutsideIt(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 3, "Windowed Partner", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	franchiseID := 3
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "windowed_sub", FranchiseID: &franchiseID})

	// One recharge two months ago, one now. A window covering only "now"
	// must report just the second.
	if _, err := pool.Exec(ctx, `
		INSERT INTO lco_ledger (franchise_id, subscriber_id, recharge_amount, commission_amount, transaction_ref, created_at)
		VALUES (3, 1, 500.00, 50.00, 'old', NOW() - INTERVAL '60 days'),
		       (3, 1, 300.00, 30.00, 'new', NOW())`); err != nil {
		t.Fatalf("seed recharges: %v", err)
	}

	from := time.Now().Add(-24 * time.Hour)
	pnl, err := database.Revenue().GetFranchisePnL(ctx, 3, &from, nil)
	if err != nil {
		t.Fatalf("GetFranchisePnL with a from-window: %v", err)
	}
	if pnl.RechargeCount != 1 {
		t.Fatalf("recharge_count = %d within the window, want 1 (the 60-day-old recharge must be excluded)", pnl.RechargeCount)
	}
	if pnl.TotalRecharges != "300.00" {
		t.Errorf("total_recharges within window = %q, want 300.00", pnl.TotalRecharges)
	}
}

// TestFR_FRN_003_ListConsolidatedPnL_TotalsEqualTheSumOfPartners is the
// property the whole endpoint exists to guarantee: a consolidated total that
// does not equal the sum of what it lists is worse than no total.
func TestFR_FRN_003_ListConsolidatedPnL_TotalsEqualTheSumOfPartners(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 1, "Partner A", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "Partner B", "20.00", "active")
	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	f1, f2 := 1, 2
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "a_sub", FranchiseID: &f1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "b_sub", FranchiseID: &f2})

	if _, err := pool.Exec(ctx, `
		INSERT INTO lco_ledger (franchise_id, subscriber_id, recharge_amount, commission_amount, transaction_ref)
		VALUES (1, 1, 1000.00, 100.00, 'a1'),
		       (2, 2, 500.00,  100.00, 'b1')`); err != nil {
		t.Fatalf("seed recharges: %v", err)
	}

	consolidated, err := database.Revenue().ListConsolidatedPnL(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListConsolidatedPnL: %v", err)
	}
	if len(consolidated.Partners) != 2 {
		t.Fatalf("partners = %d, want 2", len(consolidated.Partners))
	}
	if consolidated.TotalRecharges != "1500.00" {
		t.Errorf("total_recharges = %q, want 1500.00 (1000+500)", consolidated.TotalRecharges)
	}
	if consolidated.CommissionEarned != "200.00" {
		t.Errorf("commission_earned = %q, want 200.00 (100+100)", consolidated.CommissionEarned)
	}
	if consolidated.NetToISP != "1300.00" {
		t.Errorf("net_to_isp = %q, want 1300.00 (1500-200)", consolidated.NetToISP)
	}
}

func TestFR_FRN_004_ListFranchises_ScopedVsUnrestricted(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedFranchise(ctx, t, pool, 1, "Partner A", "10.00", "active")
	seedFranchise(ctx, t, pool, 2, "Partner B", "10.00", "active")
	seedFranchise(ctx, t, pool, 3, "Partner C", "10.00", "active")

	all, err := database.Revenue().ListFranchises(ctx, nil)
	if err != nil {
		t.Fatalf("ListFranchises(nil): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unrestricted list = %d partners, want 3", len(all))
	}

	scoped := 2
	one, err := database.Revenue().ListFranchises(ctx, &scoped)
	if err != nil {
		t.Fatalf("ListFranchises(&2): %v", err)
	}
	if len(one) != 1 || one[0].ID != 2 {
		t.Errorf("scoped list = %+v, want exactly partner 2", one)
	}
}

func TestFR_FRN_006_CreateFranchise_PersistsAndIsListable(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	req := revenue.CreateFranchiseRequest{
		Name: "Chennai North", OwnerName: "R Kumar",
		MobileNumber: "+919876500000", CommissionRatePct: "12.50",
	}
	created, err := database.Revenue().CreateFranchise(ctx, req)
	if err != nil {
		t.Fatalf("CreateFranchise: %v", err)
	}
	if created.Status != "active" {
		t.Errorf("a newly onboarded franchise must default to active, got %q", created.Status)
	}

	list, err := database.Revenue().ListFranchises(ctx, &created.ID)
	if err != nil {
		t.Fatalf("ListFranchises: %v", err)
	}
	if len(list) != 1 || list[0].Name != "Chennai North" {
		t.Errorf("created franchise not listable: %+v", list)
	}
}
