//go:build integration

// Integration tests for the live-session cache.
//
// Redis is a real server (miniredis, in-process), so key formats, TTLs and the
// JSON encoding are exercised for real rather than mocked.
package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/maaransoft/isp-bss-oss/internal/cache"
	"github.com/redis/go-redis/v9"
)

const quota3TB = int64(3_543_348_019_200)

func newStore(t *testing.T) (*cache.SessionStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return cache.NewSessionStore(rc), mr
}

func sampleSession() cache.Session {
	return cache.Session{
		SessionID:    "sess-live-001",
		SubscriberID: 42,
		NasIP:        "10.10.0.1",
		AssignedIP:   "100.64.12.7",
		BytesIn:      2_000_000_000_000,
		BytesOut:     834_678_415_360,
		BytesTotal:   quota3TB,
		SpeedProfile: "100M/100M",
		StartedAt:    time.Now().Add(-3*time.Hour - 12*time.Minute),
	}
}

// TestSessionStore_RoundTrip verifies a session survives storage and reads back
// with its derived usage figures intact.
func TestSessionStore_RoundTrip(t *testing.T) {
	store, mr := newStore(t)
	ctx := context.Background()

	sess := sampleSession()
	if err := store.Put(ctx, sess); err != nil {
		t.Fatalf("Put: %v", err)
	}

	key := cache.SessionKey(42)
	if !mr.Exists(key) {
		t.Fatalf("expected session at key %q", key)
	}
	if ttl := mr.TTL(key); ttl != cache.SessionTTL {
		t.Errorf("TTL: want %v, got %v", cache.SessionTTL, ttl)
	}

	got, err := store.Get(ctx, 42)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("want a session, got nil")
	}
	if got.SessionID != sess.SessionID || got.NasIP != sess.NasIP {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if got.BytesUsed() != sess.BytesIn+sess.BytesOut {
		t.Errorf("bytes_used: want %d, got %d", sess.BytesIn+sess.BytesOut, got.BytesUsed())
	}
	if pct := got.PctUsed(); pct != 80 {
		t.Errorf("pct_used: want 80, got %d", pct)
	}
}

// TestSessionStore_OfflineIsNotAnError verifies a missing key reads as offline
// rather than failing the caller.
func TestSessionStore_OfflineIsNotAnError(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	got, err := store.Get(ctx, 999)
	if err != nil {
		t.Fatalf("a missing session must not error, got: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for an offline subscriber, got %+v", got)
	}

	summary, err := store.GetActiveSession(ctx, 999)
	if err != nil || summary != nil {
		t.Errorf("health view: want (nil, nil), got (%+v, %v)", summary, err)
	}
	portalSession, err := store.Portal().GetActiveSession(ctx, 999)
	if err != nil || portalSession != nil {
		t.Errorf("portal view: want (nil, nil), got (%+v, %v)", portalSession, err)
	}
}

// TestSessionStore_HealthView verifies the health projection.
func TestSessionStore_HealthView(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	if err := store.Put(ctx, sampleSession()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	summary, err := store.GetActiveSession(ctx, 42)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if summary == nil {
		t.Fatal("want a summary, got nil")
	}
	if summary.SessionID != "sess-live-001" {
		t.Errorf("session_id: got %q", summary.SessionID)
	}
	if summary.AssignedIP != "100.64.12.7" {
		t.Errorf("assigned_ip: got %q", summary.AssignedIP)
	}
	if summary.BytesTotal != quota3TB {
		t.Errorf("bytes_total: want %d, got %d", quota3TB, summary.BytesTotal)
	}
	if summary.PctUsed != 80 {
		t.Errorf("pct_used: want 80, got %d", summary.PctUsed)
	}
	if summary.SessionAge != "3h12m" {
		t.Errorf("session_age: want 3h12m, got %q", summary.SessionAge)
	}
}

// TestSessionStore_PortalView verifies the portal projection reports usage in GB.
func TestSessionStore_PortalView(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	sess := sampleSession()
	sess.FUPThrottled = true
	if err := store.Put(ctx, sess); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := store.Portal().GetActiveSession(ctx, 42)
	if err != nil {
		t.Fatalf("portal GetActiveSession: %v", err)
	}
	if got == nil {
		t.Fatal("want a session, got nil")
	}
	if !got.FUPThrottled {
		t.Error("fup_throttled must carry through to the portal view")
	}
	if got.BytesIn != sess.BytesIn || got.BytesOut != sess.BytesOut {
		t.Errorf("octets: got in=%d out=%d", got.BytesIn, got.BytesOut)
	}
	// 2_834_678_415_360 bytes is ~2640.0 GiB of a 3300 GiB quota.
	if got.GBIncluded.IntPart() != 3300 {
		t.Errorf("gb_included: want 3300, got %s", got.GBIncluded)
	}
	if got.GBUsed.IntPart() != 2640 {
		t.Errorf("gb_used: want 2640, got %s", got.GBUsed)
	}
	if got.PctUsed < 79.9 || got.PctUsed > 80.1 {
		t.Errorf("pct_used: want ~80, got %v", got.PctUsed)
	}
}

// TestSessionStore_UnlimitedPlan verifies an unlimited plan never reports a
// percentage, which would otherwise divide by zero.
func TestSessionStore_UnlimitedPlan(t *testing.T) {
	store, _ := newStore(t)
	ctx := context.Background()

	sess := sampleSession()
	sess.BytesTotal = 0 // unlimited
	if err := store.Put(ctx, sess); err != nil {
		t.Fatalf("Put: %v", err)
	}

	summary, err := store.GetActiveSession(ctx, 42)
	if err != nil {
		t.Fatalf("GetActiveSession: %v", err)
	}
	if summary.PctUsed != 0 {
		t.Errorf("an unlimited plan must report 0%%, got %d", summary.PctUsed)
	}

	portalSession, err := store.Portal().GetActiveSession(ctx, 42)
	if err != nil {
		t.Fatalf("portal GetActiveSession: %v", err)
	}
	if portalSession.PctUsed != 0 {
		t.Errorf("an unlimited plan must report 0%%, got %v", portalSession.PctUsed)
	}
}

// TestSessionStore_Delete verifies Accounting-Stop clears the session.
func TestSessionStore_Delete(t *testing.T) {
	store, mr := newStore(t)
	ctx := context.Background()

	if err := store.Put(ctx, sampleSession()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Delete(ctx, 42); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if mr.Exists(cache.SessionKey(42)) {
		t.Error("session key must be gone after Delete")
	}
	got, err := store.Get(ctx, 42)
	if err != nil || got != nil {
		t.Errorf("want (nil, nil) after delete, got (%+v, %v)", got, err)
	}
}

// TestSessionStore_TTLExpiry verifies a session whose Accounting-Stop was lost
// ages out rather than showing a subscriber online forever.
func TestSessionStore_TTLExpiry(t *testing.T) {
	store, mr := newStore(t)
	ctx := context.Background()

	if err := store.Put(ctx, sampleSession()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	mr.FastForward(cache.SessionTTL + time.Second)

	got, err := store.Get(ctx, 42)
	if err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if got != nil {
		t.Error("a session past its TTL must read as offline")
	}
}

// TestSessionStore_KeyIsPerSubscriber guards the key format, which the RADIUS
// daemon writes and two readers depend on.
func TestSessionStore_KeyIsPerSubscriber(t *testing.T) {
	if got, want := cache.SessionKey(42), "session:active:42"; got != want {
		t.Errorf("key: want %q, got %q", want, got)
	}
	if cache.SessionKey(1) == cache.SessionKey(2) {
		t.Error("different subscribers must map to different keys")
	}
}
