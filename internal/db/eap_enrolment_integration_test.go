//go:build integration

// EAP enrolment persistence tests — FR-AAA-006 | MDS §4.18.
package db_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

func TestFR_AAA_006_NTHashEnrolmentRoundTrip(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "eap@isp"})

	store := database.API()

	t.Run("a fresh subscriber is not enrolled", func(t *testing.T) {
		enrolled, err := store.IsEAPEnrolled(ctx, 1)
		if err != nil {
			t.Fatalf("IsEAPEnrolled: %v", err)
		}
		if enrolled {
			t.Error("no subscriber should be EAP-enrolled by default — enrolment is opt-in")
		}
	})

	ntHash := radius.NTPasswordHash("clientPass")
	if err := store.SetNTHash(ctx, 1, ntHash); err != nil {
		t.Fatalf("SetNTHash: %v", err)
	}

	t.Run("enrolment is visible", func(t *testing.T) {
		enrolled, err := store.IsEAPEnrolled(ctx, 1)
		if err != nil {
			t.Fatalf("IsEAPEnrolled: %v", err)
		}
		if !enrolled {
			t.Error("want enrolled after SetNTHash")
		}
	})

	// The RADIUS path is what actually reads it, so verify the hash survives
	// the round trip through the auth query rather than only through the
	// enrolment one.
	t.Run("the RADIUS auth query returns the hash", func(t *testing.T) {
		sub, err := database.Radius().GetSubscriberByUsername(ctx, "eap@isp")
		if err != nil {
			t.Fatalf("GetSubscriberByUsername: %v", err)
		}
		if sub == nil {
			t.Fatal("subscriber not found")
		}
		if !bytes.Equal(sub.NTHash, ntHash) {
			t.Errorf("nt_hash round trip:\n got %X\nwant %X", sub.NTHash, ntHash)
		}
	})

	t.Run("un-enrolment clears it", func(t *testing.T) {
		if err := store.SetNTHash(ctx, 1, nil); err != nil {
			t.Fatalf("SetNTHash(nil): %v", err)
		}
		enrolled, err := store.IsEAPEnrolled(ctx, 1)
		if err != nil {
			t.Fatalf("IsEAPEnrolled: %v", err)
		}
		if enrolled {
			t.Error("want un-enrolled after clearing the hash")
		}
		sub, _ := database.Radius().GetSubscriberByUsername(ctx, "eap@isp")
		if len(sub.NTHash) != 0 {
			t.Errorf("the auth path must see no hash after un-enrolment, got %X", sub.NTHash)
		}
	})

	t.Run("an unknown subscriber reports not found", func(t *testing.T) {
		if err := store.SetNTHash(ctx, 999999, ntHash); err == nil {
			t.Error("want an error for an unknown subscriber")
		}
		if _, err := store.IsEAPEnrolled(ctx, 999999); err == nil {
			t.Error("want an error for an unknown subscriber")
		}
	})
}

// TestFR_AAA_006_SchemaRejectsWrongLengthNTHash: a wrong-length value would
// otherwise fail deep inside RFC 2759's DES step with no useful diagnostic.
func TestFR_AAA_006_SchemaRejectsWrongLengthNTHash(t *testing.T) {
	_, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "badhash@isp"})

	for _, length := range []int{8, 15, 17, 32} {
		_, err := pool.Exec(ctx, `UPDATE subscribers SET nt_hash = $2 WHERE id = $1`, 1, make([]byte, length))
		if err == nil {
			t.Errorf("chk_subscribers_nt_hash_len must reject a %d-byte hash", length)
		}
	}

	// And accept exactly 16.
	if _, err := pool.Exec(ctx, `UPDATE subscribers SET nt_hash = $2 WHERE id = $1`, 1, make([]byte, 16)); err != nil {
		t.Errorf("a 16-byte hash must be accepted: %v", err)
	}
}

// TestFR_AAA_006_PasswordHashIsReadableForVerification covers the narrow
// credential lookup enrolment uses to prove the presented password is the
// subscriber's real one.
func TestFR_AAA_006_PasswordHashIsReadableForVerification(t *testing.T) {
	database, pool := newTestDB(t)
	ctx := context.Background()

	seedPlan(ctx, t, pool, 1, "Basic", "50M/50M", 0, "", "499.00")
	seedSubscriber(ctx, t, pool, 1, seedOpts{Username: "creds@isp"})

	hash, err := database.API().GetPasswordHash(ctx, "creds@isp")
	if err != nil {
		t.Fatalf("GetPasswordHash: %v", err)
	}
	if hash == "" {
		t.Error("want the stored bcrypt hash")
	}

	if _, err := database.API().GetPasswordHash(ctx, "nobody@isp"); err == nil {
		t.Error("want an error for an unknown username")
	}
}
