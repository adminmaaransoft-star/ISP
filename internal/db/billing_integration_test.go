//go:build integration

package db_test

import (
	"context"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/billing"
	"github.com/shopspring/decimal"
)

// posting builds the two-leg posting that billing.WalletService produces, so the
// store is exercised with exactly the shape the service hands it.
func posting(subscriberID int, amount, newBalance, token string, franchiseID *int) billing.RechargePosting {
	amt := decimal.RequireFromString(amount)
	bal := decimal.RequireFromString(newBalance)
	var tokenPtr *string
	if token != "" {
		t := token
		tokenPtr = &t
	}
	return billing.RechargePosting{
		SubscriberID: subscriberID,
		Debit: billing.WalletEntry{
			SubscriberID: subscriberID,
			FranchiseID:  franchiseID,
			Account:      billing.AccountGatewayClearing,
			EntryType:    "debit",
			Amount:       amt,
			BalanceAfter: bal,
			Description:  "counter-entry: recharge",
		},
		Credit: billing.WalletEntry{
			SubscriberID:     subscriberID,
			FranchiseID:      franchiseID,
			Account:          billing.AccountSubscriberWallet,
			EntryType:        "credit",
			Amount:           amt,
			BalanceAfter:     bal,
			TransactionToken: tokenPtr,
			Description:      "recharge",
		},
		NewBalance: bal,
	}
}

// TestBillingStore_RecordRecharge verifies both ledger legs and the wallet
// balance are written, and that only the credit leg carries the token.
//
// FR-BIL-003 | INT-BIL-001
func TestBillingStore_RecordRecharge(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	franchiseID := 1
	seedFranchise(ctx, t, pool, 1, "Chennai LCO", "10.00", "active")
	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "wallet@isp", FranchiseID: &franchiseID})

	store := database.Billing()

	tx, err := store.RecordRecharge(ctx, posting(1, "799.00", "799.00", "pay_LxRr9wZ1", &franchiseID))
	if err != nil {
		t.Fatalf("RecordRecharge: %v", err)
	}
	if tx.ID == 0 {
		t.Error("returned transaction must carry the credit leg's id")
	}
	assertDecimalEqual(t, "tx.BalanceAfter", tx.BalanceAfter, "799.00")

	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM wallet_ledgers WHERE subscriber_id = 1`); n != 2 {
		t.Fatalf("want 2 ledger rows (debit + credit), got %d", n)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM wallet_ledgers WHERE account = 'subscriber_wallet' AND entry_type = 'credit'`); n != 1 {
		t.Errorf("want 1 subscriber_wallet credit leg, got %d", n)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM wallet_ledgers WHERE account = 'payment_gateway_clearing' AND entry_type = 'debit'`); n != 1 {
		t.Errorf("want 1 gateway clearing debit leg, got %d", n)
	}
	// idx_wallet_token is unique over non-null tokens, so only one leg may hold it.
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM wallet_ledgers WHERE transaction_token IS NOT NULL`); n != 1 {
		t.Errorf("exactly one leg may carry the idempotency token, got %d", n)
	}

	balance := scanString(ctx, t, pool, `SELECT wallet_balance::text FROM subscribers WHERE id = 1`)
	if got := mustDecimal(t, balance); !got.Equal(mustDecimal(t, "799.00")) {
		t.Errorf("subscribers.wallet_balance: want 799.00, got %s", got)
	}

	t.Run("balance reads back exactly", func(t *testing.T) {
		got, err := store.GetSubscriberBalance(ctx, 1)
		if err != nil {
			t.Fatalf("GetSubscriberBalance: %v", err)
		}
		assertDecimalEqual(t, "balance", got, "799.00")
	})

	t.Run("token lookup returns the original transaction", func(t *testing.T) {
		found, err := store.GetTransactionByToken(ctx, "pay_LxRr9wZ1")
		if err != nil {
			t.Fatalf("GetTransactionByToken: %v", err)
		}
		if found == nil {
			t.Fatal("want the original transaction, got nil")
		}
		if found.ID != tx.ID {
			t.Errorf("want id %d, got %d", tx.ID, found.ID)
		}
		assertDecimalEqual(t, "amount", found.Amount, "799.00")
	})

	t.Run("unseen token is not an error", func(t *testing.T) {
		found, err := store.GetTransactionByToken(ctx, "pay_never_seen")
		if err != nil || found != nil {
			t.Errorf("want (nil, nil), got (%+v, %v)", found, err)
		}
	})
}

// TestBillingStore_RecordRechargeIsAtomic verifies a duplicate token rolls the
// whole posting back, leaving neither ledger rows nor a changed balance.
//
// FR-BIL-005 | INT-BIL-002
func TestBillingStore_RecordRechargeIsAtomic(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "atomic@isp"})

	store := database.Billing()

	if _, err := store.RecordRecharge(ctx, posting(1, "799.00", "799.00", "pay_dup", nil)); err != nil {
		t.Fatalf("first RecordRecharge: %v", err)
	}

	// The same token again: the unique index must reject it and the transaction
	// must roll back, so no partial state survives.
	_, err := store.RecordRecharge(ctx, posting(1, "799.00", "1598.00", "pay_dup", nil))
	if err == nil {
		t.Fatal("expected a unique violation for a replayed transaction_token")
	}

	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM wallet_ledgers WHERE subscriber_id = 1`); n != 2 {
		t.Errorf("rolled-back posting must leave exactly the original 2 rows, got %d", n)
	}
	balance := scanString(ctx, t, pool, `SELECT wallet_balance::text FROM subscribers WHERE id = 1`)
	if got := mustDecimal(t, balance); !got.Equal(mustDecimal(t, "799.00")) {
		t.Errorf("balance must not advance on a rolled-back posting: got %s", got)
	}
}

// TestBillingStore_DecimalPrecision verifies money survives the round trip
// exactly, including values binary floating point cannot represent.
//
// FR-BIL-002 | DoD L0-007
func TestBillingStore_DecimalPrecision(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	store := database.Billing()

	amounts := []string{"0.01", "71.91", "143.82", "942.82", "1234567.89", "99999999.99"}
	for i, amount := range amounts {
		subscriberID := i + 1
		seedSubscriber(ctx, t, pool, subscriberID, seedOpts{Username: "precision" + amount + "@isp"})

		if _, err := store.RecordRecharge(ctx,
			posting(subscriberID, amount, amount, "tok_"+amount, nil)); err != nil {
			t.Fatalf("RecordRecharge %s: %v", amount, err)
		}

		got, err := store.GetSubscriberBalance(ctx, subscriberID)
		if err != nil {
			t.Fatalf("GetSubscriberBalance %s: %v", amount, err)
		}
		assertDecimalEqual(t, "balance for "+amount, got, amount)
	}
}

// TestBillingStore_Dunning verifies the dunning stage and the derived
// subscriber status move together.
//
// FR-BIL-004
func TestBillingStore_Dunning(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "dunning@isp"})

	store := database.Billing()

	state, _, err := store.GetSubscriberDunningState(ctx, 1)
	if err != nil {
		t.Fatalf("GetSubscriberDunningState: %v", err)
	}
	if state != billing.DunningActive {
		t.Errorf("initial state: want active, got %q", state)
	}

	// Drive the real state machine, which reads and writes through this store.
	if err := billing.TransitionDunning(ctx, store, 1, billing.DunningRemind7d); err != nil {
		t.Fatalf("TransitionDunning to remind_7d: %v", err)
	}
	state, _, _ = store.GetSubscriberDunningState(ctx, 1)
	if state != billing.DunningRemind7d {
		t.Errorf("after transition: want remind_7d, got %q", state)
	}
	// A reminder stage must not cut off service.
	if status := scanString(ctx, t, pool, `SELECT status FROM subscribers WHERE id = 1`); status != "active" {
		t.Errorf("status during reminders: want active, got %q", status)
	}

	t.Run("an illegal edge is refused", func(t *testing.T) {
		// remind_7d cannot jump straight to hard_suspended.
		if err := billing.TransitionDunning(ctx, store, 1, billing.DunningHardSuspended); err == nil {
			t.Error("expected an invalid-transition error")
		}
		state, _, _ := store.GetSubscriberDunningState(ctx, 1)
		if state != billing.DunningRemind7d {
			t.Errorf("a refused transition must not change state, got %q", state)
		}
	})

	t.Run("suspension propagates to subscribers.status", func(t *testing.T) {
		for _, target := range []billing.DunningState{
			billing.DunningRemind3d, billing.DunningRemind1d,
			billing.DunningGracePeriod, billing.DunningSoftSuspended,
			billing.DunningHardSuspended,
		} {
			if err := billing.TransitionDunning(ctx, store, 1, target); err != nil {
				t.Fatalf("TransitionDunning to %s: %v", target, err)
			}
		}
		if status := scanString(ctx, t, pool, `SELECT status FROM subscribers WHERE id = 1`); status != "hard_suspended" {
			t.Errorf("status: want hard_suspended so RADIUS rejects, got %q", status)
		}
	})

	t.Run("payment restores service", func(t *testing.T) {
		if err := billing.TransitionDunning(ctx, store, 1, billing.DunningActive); err != nil {
			t.Fatalf("TransitionDunning to active: %v", err)
		}
		if status := scanString(ctx, t, pool, `SELECT status FROM subscribers WHERE id = 1`); status != "active" {
			t.Errorf("status after payment: want active, got %q", status)
		}
	})

	t.Run("unknown subscriber reports not found", func(t *testing.T) {
		if _, _, err := store.GetSubscriberDunningState(ctx, 999999); err == nil {
			t.Error("want an error for an unknown subscriber")
		}
		if err := store.SetSubscriberDunningState(ctx, 999999, billing.DunningActive, "active"); err == nil {
			t.Error("want an error when updating an unknown subscriber")
		}
	})
}

// TestBillingStore_Invoices verifies invoice persistence and that the schema
// rejects an invoice carrying both intrastate and interstate tax.
//
// FR-BIL-001 | INT-BIL-006
func TestBillingStore_Invoices(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "100M/100M", 0, "", "799.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "invoice@isp"})
	seedGstRate(ctx, t, pool, 1)

	store := database.Billing()

	rate, err := store.GetActiveGstRate(ctx)
	if err != nil {
		t.Fatalf("GetActiveGstRate: %v", err)
	}
	assertDecimalEqual(t, "cgst_rate", rate.CgstRate, "9.00")
	assertDecimalEqual(t, "igst_rate", rate.IgstRate, "18.00")

	// Compute with the real function, then persist: the two must agree.
	inv := billing.CalculateGstInvoice(decimal.RequireFromString("799.00"), "TN", rate)
	inv.SubscriberID = 1
	inv.GbIncluded = 3300
	inv.GbUsed = decimal.RequireFromString("950.25")

	id, err := store.CreateInvoice(ctx, inv)
	if err != nil {
		t.Fatalf("CreateInvoice: %v", err)
	}
	if id == 0 {
		t.Error("invoice must carry a generated id")
	}

	total := scanString(ctx, t, pool, `SELECT total_amount::text FROM invoices WHERE id = $1`, id)
	if got := mustDecimal(t, total); !got.Equal(mustDecimal(t, "942.82")) {
		t.Errorf("persisted total: want 942.82, got %s", got)
	}
	cgst := scanString(ctx, t, pool, `SELECT cgst_amount::text FROM invoices WHERE id = $1`, id)
	if got := mustDecimal(t, cgst); !got.Equal(mustDecimal(t, "71.91")) {
		t.Errorf("persisted CGST: want 71.91, got %s", got)
	}

	t.Run("dual tax is rejected by chk_gst_logic", func(t *testing.T) {
		bad := inv
		bad.IgstAmount = decimal.RequireFromString("143.82") // both CGST and IGST
		if _, err := store.CreateInvoice(ctx, bad); err == nil {
			t.Error("expected chk_gst_logic to reject an invoice with both tax kinds")
		}
	})
}
