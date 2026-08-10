//go:build integration

// Integration tests for the read-through subscriber authentication cache.
//
// This file exists because subscriber.go was entirely uncovered while sitting
// directly on the RADIUS authentication hot path — the one place in this
// codebase where a caching bug is also an authentication bug. A stale or
// wrongly-served entry here does not degrade performance, it authenticates
// somebody it should not.
//
// Redis is a real server (miniredis, in-process), so key formats, TTLs, the
// JSON wire format and the read-through/fallback behaviour are exercised for
// real rather than mocked.
package cache_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/maaransoft/isp-bss-oss/internal/cache"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// fakeAuthDB is a radius.DBQuerier that counts lookups, so a test can prove a
// second request was served from Redis rather than hitting the database again.
type fakeAuthDB struct {
	mu    sync.Mutex
	sub   *radius.Subscriber
	err   error
	calls int
}

func (f *fakeAuthDB) GetSubscriberByUsername(_ context.Context, _ string) (*radius.Subscriber, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.sub, nil
}

func (f *fakeAuthDB) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newSubscriberCache(t *testing.T, db radius.DBQuerier, ttl time.Duration) (*cache.SubscriberCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return cache.NewSubscriberCache(db, rc, ttl), mr
}

func sampleAuthSubscriber() *radius.Subscriber {
	return &radius.Subscriber{
		ID:           4242,
		Username:     "sub4242",
		PasswordHash: "$2a$12$abcdefghijklmnopqrstuv",
		Status:       "active",
		RateLimitStr: "100M/100M",
		FUPActive:    false,
		FUPThrottle:  "",
	}
}

func TestFR_AAA_002_SubscriberCacheKeyFormat(t *testing.T) {
	if got := cache.SubscriberCacheKey("sub1"); got != "subscriber:auth:sub1" {
		t.Errorf("key: want subscriber:auth:sub1, got %q", got)
	}
}

// TestFR_AAA_002_SubscriberCache_MissThenHit is the core read-through
// behaviour: the first lookup reaches PostgreSQL, the second must not.
func TestFR_AAA_002_SubscriberCache_MissThenHit(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c, mr := newSubscriberCache(t, db, time.Minute)

	first, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	if first == nil || first.ID != 4242 {
		t.Fatalf("first lookup returned %+v", first)
	}
	if db.callCount() != 1 {
		t.Fatalf("want 1 DB call after a cold miss, got %d", db.callCount())
	}
	if !mr.Exists("subscriber:auth:sub4242") {
		t.Fatal("the miss should have populated Redis")
	}

	second, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if db.callCount() != 1 {
		t.Errorf("second lookup must be served from Redis; DB calls went to %d", db.callCount())
	}

	// Every auth-relevant field must survive the JSON round trip. PasswordHash
	// in particular: a field silently lost here would make the cached record
	// fail every bcrypt comparison.
	if second.ID != first.ID || second.Username != first.Username ||
		second.PasswordHash != first.PasswordHash || second.Status != first.Status ||
		second.RateLimitStr != first.RateLimitStr || second.FUPActive != first.FUPActive ||
		second.FUPThrottle != first.FUPThrottle {
		t.Errorf("cached record differs from the source:\n first  = %+v\n second = %+v", first, second)
	}
}

// TestFR_AAA_002_SubscriberCache_ThrottledFieldsRoundTrip covers the FUP fields
// specifically, since they drive the rate limit applied to a live session.
func TestFR_AAA_002_SubscriberCache_ThrottledFieldsRoundTrip(t *testing.T) {
	sub := sampleAuthSubscriber()
	sub.FUPActive = true
	sub.FUPThrottle = "2M/2M"
	db := &fakeAuthDB{sub: sub}
	c, _ := newSubscriberCache(t, db, time.Minute)

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("cached lookup: %v", err)
	}
	if !got.FUPActive || got.FUPThrottle != "2M/2M" {
		t.Errorf("FUP fields lost through the cache: FUPActive=%v FUPThrottle=%q", got.FUPActive, got.FUPThrottle)
	}
}

// TestFR_AAA_002_SubscriberCache_NegativeEntry verifies an unknown username is
// cached as a negative result, so a flood of requests for a nonexistent user
// cannot become a flood of database queries.
func TestFR_AAA_002_SubscriberCache_NegativeEntry(t *testing.T) {
	db := &fakeAuthDB{sub: nil}
	c, mr := newSubscriberCache(t, db, time.Minute)

	got, err := c.GetSubscriberByUsername(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil for an unknown username, got %+v", got)
	}
	if !mr.Exists("subscriber:auth:ghost") {
		t.Fatal("a negative result must still be cached")
	}

	got2, err := c.GetSubscriberByUsername(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("second lookup: %v", err)
	}
	if got2 != nil {
		t.Errorf("cached negative entry must still return nil, got %+v", got2)
	}
	if db.callCount() != 1 {
		t.Errorf("the negative entry should have absorbed the second lookup; DB calls = %d", db.callCount())
	}
}

// TestFR_AAA_002_SubscriberCache_NegativeEntryExpiresFaster pins the shorter
// negative TTL. Without it, a newly provisioned subscriber stays locked out for
// a full TTL because of one lookup that happened before they existed.
func TestFR_AAA_002_SubscriberCache_NegativeEntryExpiresFaster(t *testing.T) {
	const ttl = 60 * time.Second
	db := &fakeAuthDB{sub: nil}
	c, mr := newSubscriberCache(t, db, ttl)

	if _, err := c.GetSubscriberByUsername(context.Background(), "ghost"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	negTTL := mr.TTL("subscriber:auth:ghost")
	if negTTL <= 0 || negTTL > ttl/2 {
		t.Errorf("negative entry TTL should be well under the positive TTL (%s), got %s", ttl, negTTL)
	}

	// And the positive TTL for comparison, so this test fails if the two are
	// ever collapsed into one value.
	dbPos := &fakeAuthDB{sub: sampleAuthSubscriber()}
	cPos, mrPos := newSubscriberCache(t, dbPos, ttl)
	if _, err := cPos.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if posTTL := mrPos.TTL("subscriber:auth:sub4242"); posTTL <= negTTL {
		t.Errorf("positive TTL (%s) must exceed the negative TTL (%s)", posTTL, negTTL)
	}
}

// TestFR_AAA_002_SubscriberCache_TTLExpiryRefetches proves the TTL is real and
// the entry is reloaded once it lapses.
func TestFR_AAA_002_SubscriberCache_TTLExpiryRefetches(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c, mr := newSubscriberCache(t, db, 30*time.Second)

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	mr.FastForward(31 * time.Second)

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("post-expiry lookup: %v", err)
	}
	if db.callCount() != 2 {
		t.Errorf("an expired entry must be refetched from the DB; DB calls = %d, want 2", db.callCount())
	}
}

// TestFR_AAA_002_SubscriberCache_DefaultTTLApplied covers the zero/negative TTL
// guard in the constructor.
func TestFR_AAA_002_SubscriberCache_DefaultTTLApplied(t *testing.T) {
	for _, ttl := range []time.Duration{0, -5 * time.Second} {
		db := &fakeAuthDB{sub: sampleAuthSubscriber()}
		c, mr := newSubscriberCache(t, db, ttl)
		if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
			t.Fatalf("lookup with ttl=%s: %v", ttl, err)
		}
		got := mr.TTL("subscriber:auth:sub4242")
		if got <= 0 || got > cache.DefaultSubscriberTTL {
			t.Errorf("ttl=%s should fall back to DefaultSubscriberTTL (%s), got %s", ttl, cache.DefaultSubscriberTTL, got)
		}
	}
}

// TestFR_AAA_002_SubscriberCache_InvalidateForcesReload covers the path a
// suspension depends on. Without invalidation, a suspended subscriber keeps
// authenticating until the TTL lapses.
func TestFR_AAA_002_SubscriberCache_InvalidateForcesReload(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c, mr := newSubscriberCache(t, db, time.Minute)

	if _, err := c.GetSubscriberByUsername(context.Background(), "sub4242"); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if err := c.InvalidateSubscriber(context.Background(), "sub4242"); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if mr.Exists("subscriber:auth:sub4242") {
		t.Fatal("invalidate must remove the key")
	}

	// The reload must observe the *new* status, which is the entire point.
	suspended := sampleAuthSubscriber()
	suspended.Status = "hard_suspended"
	db.mu.Lock()
	db.sub = suspended
	db.mu.Unlock()

	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("post-invalidate lookup: %v", err)
	}
	if got.Status != "hard_suspended" {
		t.Errorf("status after invalidation: want hard_suspended, got %q", got.Status)
	}
}

// TestFR_AAA_002_SubscriberCache_InvalidateUnknownIsNotAnError — Redis DEL on a
// missing key is a no-op, and invalidating a subscriber who was never cached
// must not fail the caller that is suspending them.
func TestFR_AAA_002_SubscriberCache_InvalidateUnknownIsNotAnError(t *testing.T) {
	c, _ := newSubscriberCache(t, &fakeAuthDB{sub: sampleAuthSubscriber()}, time.Minute)
	if err := c.InvalidateSubscriber(context.Background(), "never-cached"); err != nil {
		t.Errorf("invalidating an uncached username should succeed, got %v", err)
	}
}

// TestFR_AAA_002_SubscriberCache_RedisDownFallsThrough is the availability
// guarantee in subscriber.go's doc comment: Redis is an optimisation, never a
// dependency. With Redis unreachable, authentication must still work.
func TestFR_AAA_002_SubscriberCache_RedisDownFallsThrough(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c, mr := newSubscriberCache(t, db, time.Minute)
	mr.Close() // every subsequent Redis call now errors

	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("auth must survive Redis being down, got error: %v", err)
	}
	if got == nil || got.ID != 4242 {
		t.Fatalf("want the record from PostgreSQL, got %+v", got)
	}
	if db.callCount() != 1 {
		t.Errorf("want the DB to have been consulted once, got %d", db.callCount())
	}
}

// TestFR_AAA_002_SubscriberCache_InvalidateReportsRedisFailure is the
// counterpart: a *read* tolerates Redis being down, but invalidation cannot
// silently succeed when it did not happen — the caller suspending a subscriber
// needs to know the stale entry may still be live.
func TestFR_AAA_002_SubscriberCache_InvalidateReportsRedisFailure(t *testing.T) {
	c, mr := newSubscriberCache(t, &fakeAuthDB{sub: sampleAuthSubscriber()}, time.Minute)
	mr.Close()

	if err := c.InvalidateSubscriber(context.Background(), "sub4242"); err == nil {
		t.Error("invalidation must report a Redis failure rather than silently succeed")
	}
}

// TestFR_AAA_002_SubscriberCache_CorruptEntryFallsThrough covers the
// treat-corrupt-as-miss path. A garbage cache entry must never fail an
// authentication that PostgreSQL can still answer.
func TestFR_AAA_002_SubscriberCache_CorruptEntryFallsThrough(t *testing.T) {
	db := &fakeAuthDB{sub: sampleAuthSubscriber()}
	c, mr := newSubscriberCache(t, db, time.Minute)

	if err := mr.Set("subscriber:auth:sub4242", "{not json"); err != nil {
		t.Fatalf("seed corrupt entry: %v", err)
	}

	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if err != nil {
		t.Fatalf("a corrupt entry must not fail the lookup, got: %v", err)
	}
	if got == nil || got.ID != 4242 {
		t.Fatalf("want the record from PostgreSQL, got %+v", got)
	}
	if db.callCount() != 1 {
		t.Errorf("want a DB fallback on a corrupt entry, DB calls = %d", db.callCount())
	}
}

// TestFR_AAA_002_SubscriberCache_DBErrorPropagates — a cache miss plus a real
// database error is a genuine failure and must not be swallowed into a nil
// subscriber, which the caller would read as "no such user".
func TestFR_AAA_002_SubscriberCache_DBErrorPropagates(t *testing.T) {
	wantErr := errors.New("connection refused")
	db := &fakeAuthDB{err: wantErr}
	c, mr := newSubscriberCache(t, db, time.Minute)

	got, err := c.GetSubscriberByUsername(context.Background(), "sub4242")
	if !errors.Is(err, wantErr) {
		t.Fatalf("want the DB error propagated, got %v", err)
	}
	if got != nil {
		t.Errorf("want nil subscriber alongside the error, got %+v", got)
	}
	if mr.Exists("subscriber:auth:sub4242") {
		t.Error("a failed DB lookup must not be cached")
	}
}
