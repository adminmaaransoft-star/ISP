//go:build integration

// NAS inventory management — FR-NAS-001..004, FR-HSP-002 | migration 022, 034.
//
// The property that matters most here is negative: the summary read must not
// return the RADIUS shared secret. Everything else on a NAS row is
// configuration; that column is a credential which, if it leaks, lets anyone
// decrypt every password on the wire between the NAS and this server.
package db_test

import (
	"context"
	"strings"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// fakeCiphertext stands in for an encrypted RADIUS secret. It is not a
// credential — it is the shape the encryptor produces, so the store can be
// exercised without this package ever handling a real secret.
const fakeCiphertext = "v1:" + "Y2lwaGVydGV4dA=="

func newNAS(ip string, allowMAB bool) nas.NewNASDevice {
	return nas.NewNASDevice{
		IP:              ip,
		Vendor:          "mikrotik",
		Description:     "Cafe hotspot",
		SecretEncrypted: fakeCiphertext,
		KeyVersion:      "v1",
		CoAPort:         1700,
		PoDPort:         1700,
		AllowMAB:        allowMAB,
	}
}

func TestFR_NAS_001_RegisterAndListWithoutExposingSecrets(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedEncryptionKey(ctx, t, pool, "v1")

	store := database.NAS()
	created, err := store.CreateNASDevice(ctx, newNAS("203.0.113.50", false))
	if err != nil {
		t.Fatalf("CreateNASDevice: %v", err)
	}
	if created.IP != "203.0.113.50" || created.Vendor != "mikrotik" {
		t.Errorf("created device: %+v", created)
	}
	if created.AllowMAB {
		t.Error("allow_mab must default off — a MAC is spoofable, so MAB is per-NAS opt-in")
	}

	summaries, err := store.ListNASDeviceSummaries(ctx)
	if err != nil {
		t.Fatalf("ListNASDeviceSummaries: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("want 1 device, got %d", len(summaries))
	}

	// The whole point of the separate summary type and query.
	blob := mustJSON(t, summaries)
	for _, forbidden := range []string{fakeCiphertext, "radius_secret", "secret"} {
		if strings.Contains(blob, forbidden) {
			t.Errorf("a NAS summary must not carry %q — the shared secret keys the MD5 stream "+
				"protecting every password on the wire; serialised: %s", forbidden, blob)
		}
	}

	// The resolver's own read still gets the ciphertext it needs to decrypt.
	rows, err := store.ListNASDevices(ctx)
	if err != nil {
		t.Fatalf("ListNASDevices: %v", err)
	}
	if len(rows) != 1 || rows[0].RadiusSecretEncrypted == "" {
		t.Error("the resolver's read must still return the encrypted secret")
	}
}

// TestFR_HSP_002_AllowMABIsTogglableWithoutTouchingTheSecret is the operational
// gap this closed: enabling MAB used to require an UPDATE at a psql prompt.
func TestFR_HSP_002_AllowMABIsTogglableWithoutTouchingTheSecret(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedEncryptionKey(ctx, t, pool, "v1")

	store := database.NAS()
	created, err := store.CreateNASDevice(ctx, newNAS("203.0.113.51", false))
	if err != nil {
		t.Fatalf("CreateNASDevice: %v", err)
	}

	on := true
	updated, err := store.UpdateNASDevice(ctx, created.ID, nas.NASDeviceUpdate{AllowMAB: &on})
	if err != nil {
		t.Fatalf("UpdateNASDevice: %v", err)
	}
	if updated == nil || !updated.AllowMAB {
		t.Fatalf("allow_mab must be enabled, got %+v", updated)
	}

	// Everything else survives an allow_mab-only patch — in particular the
	// secret, which a naive full-row update would blank.
	rows, err := store.ListNASDevices(ctx)
	if err != nil {
		t.Fatalf("ListNASDevices: %v", err)
	}
	if len(rows) != 1 || rows[0].RadiusSecretEncrypted != fakeCiphertext {
		t.Errorf("the secret must be untouched by an allow_mab toggle, got %+v", rows)
	}
	if rows[0].Vendor != "mikrotik" || rows[0].CoAPort != 1700 {
		t.Errorf("unrelated fields must be untouched, got %+v", rows[0])
	}

	// And the resolver now reports it, which is what actually enables MAB.
	if !rows[0].AllowMAB {
		t.Error("the resolver's read must see allow_mab — it is the gate the RADIUS daemon checks")
	}
}

func TestFR_NAS_001_SecretRotationMovesWithItsKeyVersion(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedEncryptionKey(ctx, t, pool, "v1")
	seedEncryptionKey(ctx, t, pool, "v2")

	store := database.NAS()
	created, err := store.CreateNASDevice(ctx, newNAS("203.0.113.52", true))
	if err != nil {
		t.Fatalf("CreateNASDevice: %v", err)
	}

	// Rotation fixture, same reasoning as fakeCiphertext above.
	rotated, newVersion := "v2:"+"bmV3Y2lwaGVy", "v2"
	if _, err := store.UpdateNASDevice(ctx, created.ID, nas.NASDeviceUpdate{
		SecretEncrypted: &rotated, KeyVersion: &newVersion,
	}); err != nil {
		t.Fatalf("rotate secret: %v", err)
	}

	var storedSecret, storedVersion string
	if err := pool.QueryRow(ctx,
		`SELECT radius_secret_encrypted, key_version_id FROM nas_devices WHERE id = $1`, created.ID).
		Scan(&storedSecret, &storedVersion); err != nil {
		t.Fatalf("read rotated secret: %v", err)
	}
	if storedSecret != rotated || storedVersion != newVersion {
		t.Errorf("ciphertext and key version must move together, got %q / %q — a mismatch makes "+
			"the secret undecryptable and takes the NAS offline at the next refresh",
			storedSecret, storedVersion)
	}
}

func TestFR_NAS_001_DuplicateIPAndMissingDeviceReportHonestly(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()
	seedEncryptionKey(ctx, t, pool, "v1")

	store := database.NAS()
	if _, err := store.CreateNASDevice(ctx, newNAS("203.0.113.53", false)); err != nil {
		t.Fatalf("CreateNASDevice: %v", err)
	}
	if _, err := store.CreateNASDevice(ctx, newNAS("203.0.113.53", false)); err == nil {
		t.Error("registering the same IP twice must fail — two rows for one device would make " +
			"which secret and vendor apply a coin toss")
	}

	on := true
	missing, err := store.UpdateNASDevice(ctx, 99999, nas.NASDeviceUpdate{AllowMAB: &on})
	if err != nil {
		t.Fatalf("UpdateNASDevice(missing): %v", err)
	}
	if missing != nil {
		t.Error("updating a device that does not exist must report not-found, not invent one")
	}
}
