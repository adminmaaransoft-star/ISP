//go:build integration

// Voucher data-cap persistence — FR-HSP-001 | migration 035.
//
// The SQL half of cap enforcement. What matters here is which grants the scan
// selects: a cap of 0 means unlimited and must never be read as "exhausted",
// and subscriber-backed grants must stay out of it entirely so a subscriber
// the FUP scanner only throttled is not disconnected by this path as well.
package db_test

import (
	"context"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/db"
	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
)

// seedRedeemedVoucher creates a voucher with the given cap and redeems it for
// mac, returning the grant id.
func seedRedeemedVoucher(ctx context.Context, t *testing.T, store *db.HotspotStore, hash, mac string, capBytes int64) int64 {
	t.Helper()
	if _, err := store.CreateVoucher(ctx, hotspot.NewVoucher{
		CodeHash: hash, CodePrefix: "HS-CAP", PlanID: 1,
		DurationMinutes: 120, DataCapBytes: capBytes, CreatedBy: "owner",
	}); err != nil {
		t.Fatalf("CreateVoucher: %v", err)
	}
	grantID, err := store.RedeemVoucher(ctx, hash, mac, nil)
	if err != nil || grantID == 0 {
		t.Fatalf("RedeemVoucher: id=%d err=%v", grantID, err)
	}
	return grantID
}

func TestFR_HSP_001_UsageIsMeteredOnTheGrant(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Hotspot", "10M/10M", 0, "", "20.00")
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)
	store := database.Hotspot()

	const mac = "AA:BB:CC:DD:EE:01"
	grantID := seedRedeemedVoucher(ctx, t, store, "hash-cap-1", mac, 1_073_741_824)

	matched, err := store.RecordGrantUsage(ctx, mac, "sess-1", "10.10.0.1", 500_000_000)
	if err != nil {
		t.Fatalf("RecordGrantUsage: %v", err)
	}
	if !matched {
		t.Fatal("a live grant must be matched")
	}

	var used int64
	var sessionID, nasIP string
	if err := pool.QueryRow(ctx, `
		SELECT bytes_used, COALESCE(session_id,''), COALESCE(host(nas_ip_address),'')
		  FROM hotspot_grants WHERE id = $1`, grantID).Scan(&used, &sessionID, &nasIP); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if used != 500_000_000 || sessionID != "sess-1" || nasIP != "10.10.0.1" {
		t.Errorf("grant state: used=%d session=%q nas=%q", used, sessionID, nasIP)
	}

	// Interim updates report a running total, so a second update assigns rather
	// than accumulates. Adding would multiply usage by the number of updates
	// and exhaust the voucher in minutes.
	if _, err := store.RecordGrantUsage(ctx, mac, "sess-1", "10.10.0.1", 800_000_000); err != nil {
		t.Fatalf("second RecordGrantUsage: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT bytes_used FROM hotspot_grants WHERE id = $1`, grantID).Scan(&used); err != nil {
		t.Fatalf("re-read grant: %v", err)
	}
	if used != 800_000_000 {
		t.Errorf("octets must be assigned, not accumulated: want 800000000, got %d", used)
	}
}

func TestFR_HSP_001_OverCapScanSelectsOnlyExhaustedVoucherGrants(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Hotspot", "10M/10M", 0, "", "20.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "sub@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)
	store := database.Hotspot()

	// Over its cap — the one that should be selected.
	const overMAC = "AA:BB:CC:DD:EE:01"
	overID := seedRedeemedVoucher(ctx, t, store, "hash-over", overMAC, 1_000_000)
	if _, err := store.RecordGrantUsage(ctx, overMAC, "sess-over", "10.10.0.1", 1_500_000); err != nil {
		t.Fatalf("meter over: %v", err)
	}

	// Under its cap.
	const underMAC = "AA:BB:CC:DD:EE:02"
	seedRedeemedVoucher(ctx, t, store, "hash-under", underMAC, 1_000_000)
	if _, err := store.RecordGrantUsage(ctx, underMAC, "sess-under", "10.10.0.1", 400_000); err != nil {
		t.Fatalf("meter under: %v", err)
	}

	// Uncapped: 0 is the column default and means unlimited. Reading it as
	// "immediately exhausted" would cut off every voucher issued without a cap.
	const uncappedMAC = "AA:BB:CC:DD:EE:03"
	seedRedeemedVoucher(ctx, t, store, "hash-uncapped", uncappedMAC, 0)
	if _, err := store.RecordGrantUsage(ctx, uncappedMAC, "sess-unl", "10.10.0.1", 9_000_000_000); err != nil {
		t.Fatalf("meter uncapped: %v", err)
	}

	// A subscriber-backed grant with heavy usage. Metered in
	// subscriber_session_history and policed by FUP; this scan must ignore it,
	// or a subscriber the other path only throttled gets disconnected here.
	const subMAC = "AA:BB:CC:DD:EE:04"
	if _, err := store.GrantForSubscriber(ctx, subMAC, 1, nil, 120); err != nil {
		t.Fatalf("GrantForSubscriber: %v", err)
	}
	if _, err := store.RecordGrantUsage(ctx, subMAC, "sess-sub", "10.10.0.1", 9_000_000_000); err != nil {
		t.Fatalf("meter subscriber grant: %v", err)
	}

	over, err := store.ListGrantsOverCap(ctx, 100)
	if err != nil {
		t.Fatalf("ListGrantsOverCap: %v", err)
	}
	if len(over) != 1 {
		t.Fatalf("want exactly the exhausted voucher grant, got %d: %+v", len(over), over)
	}
	if over[0].GrantID != overID {
		t.Errorf("want grant %d, got %d", overID, over[0].GrantID)
	}
	if over[0].SessionID != "sess-over" || over[0].NASIP != "10.10.0.1" {
		t.Errorf("the scan must carry what a disconnect needs, got %+v", over[0])
	}
	if over[0].CapBytes != 1_000_000 || over[0].BytesUsed != 1_500_000 {
		t.Errorf("usage/cap: %+v", over[0])
	}
}

// TestFR_HSP_001_ExhaustionIsAConditionalClaim — two scanner replicas must not
// both count the same exhaustion, and the loser needs to know it lost.
func TestFR_HSP_001_ExhaustionIsAConditionalClaim(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Hotspot", "10M/10M", 0, "", "20.00")
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)
	store := database.Hotspot()

	const mac = "AA:BB:CC:DD:EE:05"
	grantID := seedRedeemedVoucher(ctx, t, store, "hash-race", mac, 1_000)
	if _, err := store.RecordGrantUsage(ctx, mac, "sess-race", "10.10.0.1", 5_000); err != nil {
		t.Fatalf("meter: %v", err)
	}

	const racers = 8
	results := make(chan bool, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func() {
			<-start
			ok, err := store.MarkGrantExhausted(ctx, grantID)
			if err != nil {
				t.Errorf("MarkGrantExhausted: %v", err)
			}
			results <- ok
		}()
	}
	close(start)

	won := 0
	for i := 0; i < racers; i++ {
		if <-results {
			won++
		}
	}
	if won != 1 {
		t.Fatalf("exactly one scan may claim an exhaustion, %d of %d did", won, racers)
	}

	// exhausted_at distinguishes "your data ran out" from "your time ran out" —
	// revoked_at alone cannot, and the two need different answers at a counter.
	var exhausted, revoked *string
	if err := pool.QueryRow(ctx,
		`SELECT exhausted_at::text, revoked_at::text FROM hotspot_grants WHERE id = $1`, grantID).
		Scan(&exhausted, &revoked); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if exhausted == nil || revoked == nil {
		t.Errorf("both timestamps must be set: exhausted=%v revoked=%v", exhausted, revoked)
	}

	// And it stops authorising immediately — the property that actually ends
	// access, since the NAS re-authenticates the MAC.
	sub, err := store.AuthorizeMAC(ctx, mac, 1)
	if err != nil {
		t.Fatalf("AuthorizeMAC: %v", err)
	}
	if sub != nil {
		t.Error("an exhausted voucher must stop authorising, or the device simply reconnects")
	}

	// It also drops out of the scan rather than being swept forever.
	over, err := store.ListGrantsOverCap(ctx, 100)
	if err != nil {
		t.Fatalf("ListGrantsOverCap: %v", err)
	}
	if len(over) != 0 {
		t.Errorf("a revoked grant must not remain in the over-cap scan, got %d", len(over))
	}
}

// TestFR_HSP_001_MeteringIgnoresDeadGrants — usage arriving for a voucher that
// already expired or was revoked must not resurrect it.
func TestFR_HSP_001_MeteringIgnoresDeadGrants(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Hotspot", "10M/10M", 0, "", "20.00")
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)
	store := database.Hotspot()

	const mac = "AA:BB:CC:DD:EE:06"
	grantID := seedRedeemedVoucher(ctx, t, store, "hash-dead", mac, 1_000_000)
	if ok, err := store.RevokeGrant(ctx, grantID); err != nil || !ok {
		t.Fatalf("RevokeGrant: ok=%v err=%v", ok, err)
	}

	matched, err := store.RecordGrantUsage(ctx, mac, "sess-dead", "10.10.0.1", 5_000_000)
	if err != nil {
		t.Fatalf("RecordGrantUsage: %v", err)
	}
	if matched {
		t.Error("a revoked grant must not accept usage — the caller counts these as unmatched, " +
			"which is how sessions outliving their authorisation become visible")
	}

	// And an unknown MAC likewise.
	if matched, err := store.RecordGrantUsage(ctx, "FF:FF:FF:FF:FF:FF", "s", "10.10.0.1", 1); err != nil {
		t.Fatalf("RecordGrantUsage(unknown): %v", err)
	} else if matched {
		t.Error("an unknown MAC must not match a grant")
	}
}
