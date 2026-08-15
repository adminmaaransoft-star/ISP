//go:build integration

// Partner API persistence tests — FR-API-001..003 | migration 033 | MDS §4.22.
package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/maaransoft/isp-bss-oss/internal/partner"
)

func TestFR_API_001_KeyAuthenticationAndRevocation(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Partner()

	gen, err := partner.GenerateKey(partner.KeyEnvLive)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	key, err := store.CreateAPIKey(ctx, "AcmeCRM", gen.Prefix, gen.Hash,
		[]string{partner.ScopeReadSubscribers, partner.ScopeManageWebhooks}, nil, "owner")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	authed, err := store.AuthenticateKey(ctx, gen.Plaintext)
	if err != nil {
		t.Fatalf("AuthenticateKey: %v", err)
	}
	if authed == nil || authed.ID != key.ID {
		t.Fatal("a valid key must authenticate")
	}
	if !partner.HasScope(authed.Scopes, partner.ScopeReadSubscribers) {
		t.Error("scopes must survive the round trip")
	}

	// Every failure mode must be indistinguishable from the outside.
	other, _ := partner.GenerateKey(partner.KeyEnvLive)
	for name, presented := range map[string]string{
		"unknown prefix": other.Plaintext,
		"malformed":      "not-a-key",
		"empty":          "",
	} {
		got, err := store.AuthenticateKey(ctx, presented)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", name, err)
		}
		if got != nil {
			t.Errorf("%s must not authenticate", name)
		}
	}

	// Revocation is an atomic conditional claim: the second call reports
	// honestly that it changed nothing rather than overwriting revoked_at.
	revoked, err := store.RevokeAPIKey(ctx, key.ID)
	if err != nil || !revoked {
		t.Fatalf("first revoke should succeed: revoked=%v err=%v", revoked, err)
	}
	again, err := store.RevokeAPIKey(ctx, key.ID)
	if err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if again {
		t.Error("revoking an already-revoked key must report false, or revoked_at is overwritten " +
			"and when the key actually stopped working is lost")
	}

	// The property that makes revocation mean anything.
	after, err := store.AuthenticateKey(ctx, gen.Plaintext)
	if err != nil {
		t.Fatalf("AuthenticateKey after revoke: %v", err)
	}
	if after != nil {
		t.Error("a revoked key must not authenticate")
	}
}

func TestFR_API_001_ExpiredKeyIsRefused(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()
	store := database.Partner()

	gen, _ := partner.GenerateKey(partner.KeyEnvLive)
	past := time.Now().Add(-time.Hour)
	if _, err := store.CreateAPIKey(ctx, "Expired", gen.Prefix, gen.Hash,
		[]string{partner.ScopeReadSubscribers}, &past, "owner"); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	got, err := store.AuthenticateKey(ctx, gen.Plaintext)
	if err != nil {
		t.Fatalf("AuthenticateKey: %v", err)
	}
	if got != nil {
		t.Error("an expired key must fail exactly like a revoked one — it is still flagged active " +
			"in the row, so only the expiry check stops it")
	}
}

func TestFR_API_001_ScopelessKeyIsRejectedByTheDatabase(t *testing.T) {
	database, _ := newTestDB(t)
	ctx := context.Background()

	gen, _ := partner.GenerateKey(partner.KeyEnvLive)
	_, err := database.Partner().CreateAPIKey(ctx, "NoScopes", gen.Prefix, gen.Hash,
		[]string{}, nil, "owner")
	if err == nil {
		t.Error("a key with an empty scope array must be refused by chk_api_key_scoped — " +
			"cardinality(), because array_length of an empty array is NULL and a CHECK passes on NULL")
	}
}

// TestFR_API_002_SubscribersForRespectsKeyState is the property that makes
// revocation complete: silencing a partner's key must silence their webhooks.
func TestFR_API_002_SubscribersForRespectsKeyState(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	store := database.Partner()
	seedEncryptionKey(ctx, t, pool, "v1")

	gen, _ := partner.GenerateKey(partner.KeyEnvLive)
	key, err := store.CreateAPIKey(ctx, "Acme", gen.Prefix, gen.Hash,
		[]string{partner.ScopeManageWebhooks}, nil, "owner")
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	if _, err := store.CreateWebhookEndpoint(ctx, key.ID, "https://hooks.example.com/isp",
		"v1:ciphertext", "v1", []string{partner.EventTicketCreated}, "primary"); err != nil {
		t.Fatalf("CreateWebhookEndpoint: %v", err)
	}

	subs, err := store.SubscribersFor(ctx, partner.EventTicketCreated)
	if err != nil {
		t.Fatalf("SubscribersFor: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("want one subscribed endpoint, got %d", len(subs))
	}

	// An event nobody subscribed to reaches nobody.
	none, err := store.SubscribersFor(ctx, partner.EventPaymentReceived)
	if err != nil {
		t.Fatalf("SubscribersFor: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("an unsubscribed event must fan out to nobody, got %d", len(none))
	}

	if _, err := store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatalf("RevokeAPIKey: %v", err)
	}
	afterRevoke, err := store.SubscribersFor(ctx, partner.EventTicketCreated)
	if err != nil {
		t.Fatalf("SubscribersFor after revoke: %v", err)
	}
	if len(afterRevoke) != 0 {
		t.Error("revoking a key must stop its webhooks too, or revocation is only half a revocation")
	}
}

// TestFR_API_003_DeliveryLogIsIdempotent covers the unique index: a retry must
// update the attempt trail, never fork it.
func TestFR_API_003_DeliveryLogIsIdempotent(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	store := database.Partner()
	seedEncryptionKey(ctx, t, pool, "v1")

	gen, _ := partner.GenerateKey(partner.KeyEnvLive)
	key, _ := store.CreateAPIKey(ctx, "Acme", gen.Prefix, gen.Hash,
		[]string{partner.ScopeManageWebhooks}, nil, "owner")
	endpoint, err := store.CreateWebhookEndpoint(ctx, key.ID, "https://hooks.example.com/isp",
		"v1:ct", "v1", []string{partner.EventTicketCreated}, "")
	if err != nil {
		t.Fatalf("CreateWebhookEndpoint: %v", err)
	}

	ev, _ := partner.NewEvent(partner.EventTicketCreated, 7, time.Now())
	status500 := 500
	status200 := 200

	// Two failures then a success, all for the same event.
	for i := 0; i < 2; i++ {
		if err := store.RecordDeliveryAttempt(ctx, endpoint.ID, ev, partner.StatusPending,
			&status500, "upstream error", "partner returned HTTP 500", nil); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if err := store.RecordDeliveryAttempt(ctx, endpoint.ID, ev, partner.StatusDelivered,
		&status200, "ok", "", nil); err != nil {
		t.Fatalf("final attempt: %v", err)
	}

	deliveries, err := store.ListDeliveries(ctx, endpoint.ID, 50)
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("three attempts at one event must be one row, got %d — a forked log makes "+
			"the attempt count useless for spotting a flapping partner", len(deliveries))
	}
	d := deliveries[0]
	if d.Attempts != 3 {
		t.Errorf("attempts: want 3, got %d", d.Attempts)
	}
	if d.Status != partner.StatusDelivered {
		t.Errorf("status: want delivered, got %q", d.Status)
	}
	if d.DeliveredAt == nil {
		t.Error("delivered_at must be set once the delivery succeeded")
	}
	if d.EventID != ev.EventID {
		t.Error("the event id a partner saw must be the one recorded, or a support query cannot match it")
	}

	// A different event is a different row.
	other, _ := partner.NewEvent(partner.EventTicketCreated, 8, time.Now())
	if err := store.RecordDeliveryAttempt(ctx, endpoint.ID, other, partner.StatusDelivered,
		&status200, "", "", nil); err != nil {
		t.Fatalf("second event: %v", err)
	}
	deliveries, _ = store.ListDeliveries(ctx, endpoint.ID, 50)
	if len(deliveries) != 2 {
		t.Errorf("a distinct event must get its own row, got %d rows", len(deliveries))
	}
}

// TestFR_API_002_EndpointDeactivationIsScopedToItsOwner stops one partner
// disabling another's webhooks by guessing an integer.
func TestFR_API_002_EndpointDeactivationIsScopedToItsOwner(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	store := database.Partner()
	seedEncryptionKey(ctx, t, pool, "v1")

	genA, _ := partner.GenerateKey(partner.KeyEnvLive)
	keyA, _ := store.CreateAPIKey(ctx, "PartnerA", genA.Prefix, genA.Hash,
		[]string{partner.ScopeManageWebhooks}, nil, "owner")
	genB, _ := partner.GenerateKey(partner.KeyEnvLive)
	keyB, _ := store.CreateAPIKey(ctx, "PartnerB", genB.Prefix, genB.Hash,
		[]string{partner.ScopeManageWebhooks}, nil, "owner")

	epA, err := store.CreateWebhookEndpoint(ctx, keyA.ID, "https://a.example.com/hook",
		"v1:ct", "v1", []string{partner.EventTicketCreated}, "")
	if err != nil {
		t.Fatalf("CreateWebhookEndpoint: %v", err)
	}

	ok, err := store.DeactivateWebhookEndpoint(ctx, epA.ID, keyB.ID)
	if err != nil {
		t.Fatalf("DeactivateWebhookEndpoint: %v", err)
	}
	if ok {
		t.Fatal("partner B must not be able to deactivate partner A's endpoint")
	}

	// And the owner still can.
	ok, err = store.DeactivateWebhookEndpoint(ctx, epA.ID, keyA.ID)
	if err != nil || !ok {
		t.Fatalf("the owning partner must be able to deactivate: ok=%v err=%v", ok, err)
	}
}
