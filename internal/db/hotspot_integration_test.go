//go:build integration

// Hotspot persistence tests — FR-HSP-001..003 | migration 034 | MDS §4.23.
//
// MAB authenticates on a spoofable identifier, so these tests are about the
// gates that bound it: which MACs may authenticate, on which NAS, and whether
// the ordinary subscriber-status rules still apply.
package db_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
)

func seedNAS(ctx context.Context, t *testing.T, pool *pgxpool.Pool, id int, ip string, allowMAB bool) {
	t.Helper()
	seedEncryptionKey(ctx, t, pool, "v1")
	_, err := pool.Exec(ctx, `
		INSERT INTO nas_devices (id, ip, vendor, radius_secret_encrypted, key_version_id, allow_mab)
		VALUES ($1, $2::inet, 'mikrotik', 'v1:ct', 'v1', $3)`, id, ip, allowMAB)
	if err != nil {
		t.Fatalf("seed nas %d: %v", id, err)
	}
}

func TestFR_HSP_002_RegisteredMACAuthenticates(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Hotspot_10M", "10M/10M", 0, "", "99.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "cafe@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)

	store := database.Hotspot()
	if _, err := store.RegisterDevice(ctx, "AA:BB:CC:DD:EE:FF", 1, "phone", nil); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	sub, err := store.AuthorizeMAC(ctx, "AA:BB:CC:DD:EE:FF", 1)
	if err != nil {
		t.Fatalf("AuthorizeMAC: %v", err)
	}
	if sub == nil {
		t.Fatal("a registered, active MAC must authenticate")
	}
	if sub.ID != 1 {
		t.Errorf("subscriber id: want 1, got %d", sub.ID)
	}
	// The property behind FR-HSP-003: the rate limit comes from the plan, so a
	// hotspot session is shaped by the same value a PPPoE session would be.
	if sub.RateLimitStr != "10M/10M" {
		t.Errorf("rate limit must come from the plan, got %q", sub.RateLimitStr)
	}

	// An unknown MAC is refused, not defaulted.
	unknown, err := store.AuthorizeMAC(ctx, "11:22:33:44:55:66", 1)
	if err != nil {
		t.Fatalf("AuthorizeMAC(unknown): %v", err)
	}
	if unknown != nil {
		t.Error("an unregistered MAC must not authenticate — MAB is not an open door on an enabled NAS")
	}
}

// TestFR_HSP_002_DeviceIsBoundToItsNAS stops a device enrolled on one
// operator's hotspot authenticating on another's that also has MAB enabled.
func TestFR_HSP_002_DeviceIsBoundToItsNAS(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "10M/10M", 0, "", "99.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "bound@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)
	seedNAS(ctx, t, pool, 2, "203.0.113.20", true)

	store := database.Hotspot()
	nasOne := 1
	if _, err := store.RegisterDevice(ctx, "AA:BB:CC:DD:EE:FF", 1, "phone", &nasOne); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}

	onOwn, err := store.AuthorizeMAC(ctx, "AA:BB:CC:DD:EE:FF", 1)
	if err != nil || onOwn == nil {
		t.Fatalf("the device must authenticate on its own NAS: %v", err)
	}

	onOther, err := store.AuthorizeMAC(ctx, "AA:BB:CC:DD:EE:FF", 2)
	if err != nil {
		t.Fatalf("AuthorizeMAC(other NAS): %v", err)
	}
	if onOther != nil {
		t.Error("a device bound to one NAS must not authenticate on another — otherwise anyone " +
			"who learns a MAC can use it at any hotspot the operator runs")
	}
}

func TestFR_HSP_002_DeactivatedDeviceStopsAuthenticating(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "10M/10M", 0, "", "99.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "gone@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)

	store := database.Hotspot()
	if _, err := store.RegisterDevice(ctx, "AA:BB:CC:DD:EE:FF", 1, "", nil); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	ok, err := store.DeactivateDevice(ctx, "AA:BB:CC:DD:EE:FF")
	if err != nil || !ok {
		t.Fatalf("DeactivateDevice: ok=%v err=%v", ok, err)
	}

	sub, err := store.AuthorizeMAC(ctx, "AA:BB:CC:DD:EE:FF", 1)
	if err != nil {
		t.Fatalf("AuthorizeMAC: %v", err)
	}
	if sub != nil {
		t.Error("a deactivated device must stop authenticating — a lost phone is deregistered, " +
			"and that has to take effect")
	}
}

// TestFR_HSP_001_VoucherIsSingleUseUnderConcurrency covers the conditional
// claim. A printed code typed by two people at once must admit exactly one.
func TestFR_HSP_001_VoucherIsSingleUseUnderConcurrency(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Voucher_5M", "5M/5M", 0, "", "20.00")
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)

	store := database.Hotspot()
	if _, err := store.CreateVoucher(ctx, hotspot.NewVoucher{
		CodeHash: "hash-abc", CodePrefix: "HS-ABCD", PlanID: 1,
		DurationMinutes: 60, BatchRef: "batch-1", CreatedBy: "owner",
	}); err != nil {
		t.Fatalf("CreateVoucher: %v", err)
	}

	const racers = 10
	type result struct {
		grantID int64
		err     error
	}
	results := make(chan result, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		go func(n int) {
			<-start
			mac, _ := macForRacer(n)
			id, err := store.RedeemVoucher(ctx, "hash-abc", mac, nil)
			results <- result{id, err}
		}(i)
	}
	close(start)

	redeemed := 0
	for i := 0; i < racers; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("RedeemVoucher: %v", r.err)
		}
		if r.grantID != 0 {
			redeemed++
		}
	}
	if redeemed != 1 {
		t.Fatalf("a voucher must be redeemable exactly once; %d of %d concurrent redemptions "+
			"succeeded — a printed code would otherwise be shareable around a room", redeemed, racers)
	}

	// And it stays spent afterwards.
	again, err := store.RedeemVoucher(ctx, "hash-abc", "FF:EE:DD:CC:BB:AA", nil)
	if err != nil {
		t.Fatalf("RedeemVoucher(again): %v", err)
	}
	if again != 0 {
		t.Error("a spent voucher must not be redeemable again")
	}
}

// TestFR_HSP_001_UnredeemedVoucherGoesStale covers expires_at, which is the
// shelf life of the printed code itself — not the session it buys. A batch
// printed for a festival must stop working after the festival, or a stack
// found in a drawer two years later is still free service.
func TestFR_HSP_001_UnredeemedVoucherGoesStale(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "5M/5M", 0, "", "20.00")
	store := database.Hotspot()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(24 * time.Hour)
	if _, err := store.CreateVoucher(ctx, hotspot.NewVoucher{
		CodeHash: "hash-stale", CodePrefix: "HS-STAL", PlanID: 1,
		DurationMinutes: 60, ExpiresAt: &past, CreatedBy: "owner",
	}); err != nil {
		t.Fatalf("CreateVoucher(stale): %v", err)
	}
	if _, err := store.CreateVoucher(ctx, hotspot.NewVoucher{
		CodeHash: "hash-fresh", CodePrefix: "HS-FRSH", PlanID: 1,
		DurationMinutes: 60, ExpiresAt: &future, CreatedBy: "owner",
	}); err != nil {
		t.Fatalf("CreateVoucher(fresh): %v", err)
	}

	stale, err := store.RedeemVoucher(ctx, "hash-stale", "AA:BB:CC:DD:EE:10", nil)
	if err != nil {
		t.Fatalf("RedeemVoucher(stale): %v", err)
	}
	if stale != 0 {
		t.Error("a voucher past its expires_at must not redeem — an unexpiring printed code " +
			"is free service for as long as the paper survives")
	}

	fresh, err := store.RedeemVoucher(ctx, "hash-fresh", "AA:BB:CC:DD:EE:11", nil)
	if err != nil || fresh == 0 {
		t.Fatalf("a voucher inside its shelf life must redeem: id=%d err=%v", fresh, err)
	}
}

// TestFR_HSP_001_VoucherListingNeverExposesTheCode is the property that makes
// hashing the codes worth anything: a staff listing must not hand back what it
// takes to redeem them.
func TestFR_HSP_001_VoucherListingNeverExposesTheCode(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "5M/5M", 0, "", "20.00")
	store := database.Hotspot()

	generated, err := hotspot.GenerateCode()
	if err != nil {
		t.Fatalf("GenerateCode: %v", err)
	}
	id, err := store.CreateVoucher(ctx, hotspot.NewVoucher{
		CodeHash: generated.Hash, CodePrefix: generated.Prefix, PlanID: 1,
		DurationMinutes: 30, BatchRef: "festival", CreatedBy: "owner",
	})
	if err != nil {
		t.Fatalf("CreateVoucher: %v", err)
	}

	listed, err := store.ListVouchers(ctx, hotspot.VoucherFilter{BatchRef: "festival"})
	if err != nil {
		t.Fatalf("ListVouchers: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("want 1 voucher in the batch, got %d", len(listed))
	}
	if listed[0].CodePrefix != generated.Prefix {
		t.Errorf("code_prefix: want %q, got %q", generated.Prefix, listed[0].CodePrefix)
	}
	// The listing is a struct, so the code cannot appear in it by construction —
	// which is the point. Serialising it is how a reviewer sees that stays true
	// if a field is ever added.
	blob := mustJSON(t, listed)
	if contains(blob, generated.Plaintext) || contains(blob, generated.Hash) {
		t.Error("a voucher listing must expose neither the code nor its hash — either one " +
			"turns read access to the admin API into free service")
	}

	// A batch that has not been handed out yet can be pulled from circulation.
	voided, err := store.VoidVoucher(ctx, id)
	if err != nil || !voided {
		t.Fatalf("VoidVoucher: ok=%v err=%v", voided, err)
	}
	spent, err := store.RedeemVoucher(ctx, generated.Hash, "AA:BB:CC:DD:EE:12", nil)
	if err != nil {
		t.Fatalf("RedeemVoucher(voided): %v", err)
	}
	if spent != 0 {
		t.Error("a voided voucher must not redeem")
	}

	// And voiding is not a way to un-redeem: a claimed voucher stays claimed.
	if _, err := store.CreateVoucher(ctx, hotspot.NewVoucher{
		CodeHash: "hash-claimed", CodePrefix: "HS-CLMD", PlanID: 1,
		DurationMinutes: 30, CreatedBy: "owner",
	}); err != nil {
		t.Fatalf("CreateVoucher(claimed): %v", err)
	}
	claimedList, err := store.ListVouchers(ctx, hotspot.VoucherFilter{Status: "unused"})
	if err != nil {
		t.Fatalf("ListVouchers(unused): %v", err)
	}
	if len(claimedList) != 1 {
		t.Fatalf("want 1 unused voucher after voiding the other, got %d", len(claimedList))
	}
	if _, err := store.RedeemVoucher(ctx, "hash-claimed", "AA:BB:CC:DD:EE:13", nil); err != nil {
		t.Fatalf("RedeemVoucher: %v", err)
	}
	if voidedAfter, err := store.VoidVoucher(ctx, claimedList[0].ID); err != nil || voidedAfter {
		t.Errorf("a redeemed voucher must not be voidable: ok=%v err=%v — voiding it would "+
			"strand a live grant behind a voucher claiming it was never used", voidedAfter, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func macForRacer(n int) (string, bool) {
	hex := "0123456789ABCDEF"
	return "AA:BB:CC:DD:EE:" + string([]byte{hex[n/16], hex[n%16]}), true
}

// TestFR_HSP_001_GrantRespectsSubscriberStatus keeps the captive portal from
// becoming a way around suspension.
func TestFR_HSP_001_GrantRespectsSubscriberStatus(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "10M/10M", 0, "", "99.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "ok@isp", PlanID: 1})
	seedSubscriber(ctx, t, pool, 2, seedOpts{Username: "susp@isp", PlanID: 1, Status: "hard_suspended"})
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)

	store := database.Hotspot()
	good, err := store.GrantForSubscriber(ctx, "AA:BB:CC:DD:EE:01", 1, nil, 60)
	if err != nil || good == 0 {
		t.Fatalf("an active subscriber must get a grant: id=%d err=%v", good, err)
	}

	bad, err := store.GrantForSubscriber(ctx, "AA:BB:CC:DD:EE:02", 2, nil, 60)
	if err != nil {
		t.Fatalf("GrantForSubscriber(suspended): %v", err)
	}
	if bad != 0 {
		t.Error("a hard-suspended subscriber must not get a captive-portal grant — otherwise " +
			"suspension is bypassable by connecting over the hotspot")
	}

	// The granted MAC authenticates for MAB even though nothing was
	// pre-registered — that is the walk-up path FR-HSP-001 exists for.
	sub, err := store.AuthorizeMAC(ctx, "AA:BB:CC:DD:EE:01", 1)
	if err != nil {
		t.Fatalf("AuthorizeMAC via grant: %v", err)
	}
	if sub == nil {
		t.Error("a live captive-portal grant must authorise the MAC it was issued for")
	}
}

// TestFR_HSP_001_ExpiredGrantStopsAuthorising is what makes a time-limited
// voucher actually time-limited.
func TestFR_HSP_001_ExpiredGrantStopsAuthorising(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "P", "10M/10M", 0, "", "99.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "exp@isp", PlanID: 1})
	seedNAS(ctx, t, pool, 1, "203.0.113.10", true)

	store := database.Hotspot()
	grantID, err := store.GrantForSubscriber(ctx, "AA:BB:CC:DD:EE:03", 1, nil, 60)
	if err != nil || grantID == 0 {
		t.Fatalf("GrantForSubscriber: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`UPDATE hotspot_grants SET expires_at = NOW() - INTERVAL '1 minute' WHERE id = $1`, grantID); err != nil {
		t.Fatalf("expire grant: %v", err)
	}

	sub, err := store.AuthorizeMAC(ctx, "AA:BB:CC:DD:EE:03", 1)
	if err != nil {
		t.Fatalf("AuthorizeMAC: %v", err)
	}
	if sub != nil {
		t.Error("an expired grant must stop authorising — a 60-minute voucher that works forever " +
			"is not a 60-minute voucher")
	}

	// Revocation has the same effect, immediately.
	fresh, _ := store.GrantForSubscriber(ctx, "AA:BB:CC:DD:EE:04", 1, nil, 60)
	if ok, err := store.RevokeGrant(ctx, fresh); err != nil || !ok {
		t.Fatalf("RevokeGrant: ok=%v err=%v", ok, err)
	}
	revoked, err := store.AuthorizeMAC(ctx, "AA:BB:CC:DD:EE:04", 1)
	if err != nil {
		t.Fatalf("AuthorizeMAC after revoke: %v", err)
	}
	if revoked != nil {
		t.Error("a revoked grant must stop authorising immediately")
	}
}
