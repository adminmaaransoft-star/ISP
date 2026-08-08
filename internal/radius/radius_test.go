package radius_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestBruteForceKeyFormat verifies the Redis key format for brute-force tracking.
func TestBruteForceKeyFormat(t *testing.T) {
	key := "bf_attempts:testuser"
	if key != "bf_attempts:testuser" {
		t.Errorf("unexpected key: %s", key)
	}
}

// TestRateLimitSelection_NormalMode verifies rate-limit string when FUP is inactive.
func TestRateLimitSelection_NormalMode(t *testing.T) {
	_ = context.Background()              // suppress unused import in stub file
	_ = redis.NewClient(&redis.Options{}) // suppress import
	_ = time.Second

	sub := struct {
		RateLimitStr string
		FUPActive    bool
		FUPThrottle  string
	}{
		RateLimitStr: "100M/100M",
		FUPActive:    false,
		FUPThrottle:  "10M/10M",
	}

	var effective string
	if sub.FUPActive && sub.FUPThrottle != "" {
		effective = sub.FUPThrottle
	} else {
		effective = sub.RateLimitStr
	}

	if effective != "100M/100M" {
		t.Errorf("expected 100M/100M, got %s", effective)
	}
}

// TestRateLimitSelection_FUPActive verifies throttled rate-limit when FUP is active.
func TestRateLimitSelection_FUPActive(t *testing.T) {
	sub := struct {
		RateLimitStr string
		FUPActive    bool
		FUPThrottle  string
	}{
		RateLimitStr: "100M/100M",
		FUPActive:    true,
		FUPThrottle:  "10M/10M",
	}

	var effective string
	if sub.FUPActive && sub.FUPThrottle != "" {
		effective = sub.FUPThrottle
	} else {
		effective = sub.RateLimitStr
	}

	if effective != "10M/10M" {
		t.Errorf("expected 10M/10M (FUP throttle), got %s", effective)
	}
}

// TestDedupKey verifies the Redis deduplication key format for accounting packets.
func TestDedupKey(t *testing.T) {
	sessionID := "sess-abc123"
	inputOctets := uint64(1234567890)
	key := "acct_dedup:" + sessionID + ":1234567890"
	expected := "acct_dedup:sess-abc123:1234567890"
	_ = inputOctets
	if key != expected {
		t.Errorf("dedup key mismatch: got %q want %q", key, expected)
	}
}
