package radius

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
)

// Brute-force rate limiter metrics (FR-SEC-001)
var (
	bruteForceBlocked = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_bruteforce_blocked_total",
		Help: "Authentication requests blocked by brute-force limiter",
	})
)

const (
	MaxFailedAttempts = 10               // block after 10 consecutive failures
	LockoutDuration   = 15 * time.Minute // lockout window
)

// BruteForceGuard enforces per-username attempt limits via Redis.
//
// FR: FR-SEC-001 | DDS §5.1
type BruteForceGuard struct {
	rc redis.UniversalClient
}

// NewBruteForceGuard constructs a BruteForceGuard backed by rc.
func NewBruteForceGuard(rc redis.UniversalClient) *BruteForceGuard {
	return &BruteForceGuard{rc: rc}
}

// Check reports whether username is locked out, and whether any failure counter
// exists at all.
//
// hasFailures lets the caller skip the reset DELETE after a successful
// authentication when there is nothing to reset. On the hot path — a valid login
// with no prior failures, which is the overwhelming majority — that removes a
// Redis round-trip per request.
func (g *BruteForceGuard) Check(ctx context.Context, username string) (blocked, hasFailures bool, err error) {
	if g == nil || g.rc == nil {
		return false, false, nil
	}
	val, err := g.rc.Get(ctx, BruteForceKey(username)).Result()
	if errors.Is(err, redis.Nil) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("bruteforce: read counter for %q: %w", username, err)
	}
	count, convErr := strconv.Atoi(val)
	if convErr != nil {
		// A corrupt counter must not lock a subscriber out permanently, but it
		// should still be cleared on the next success.
		return false, true, nil
	}
	if count >= MaxFailedAttempts {
		bruteForceBlocked.Inc()
		return true, true, nil
	}
	return false, true, nil
}

// IsBlocked reports whether username has reached MaxFailedAttempts.
func (g *BruteForceGuard) IsBlocked(ctx context.Context, username string) (bool, error) {
	blocked, _, err := g.Check(ctx, username)
	return blocked, err
}

// RecordFailure increments the failure counter and refreshes its lockout TTL.
func (g *BruteForceGuard) RecordFailure(ctx context.Context, username string) error {
	if g == nil || g.rc == nil {
		return nil
	}
	key := BruteForceKey(username)
	if err := g.rc.Incr(ctx, key).Err(); err != nil {
		return fmt.Errorf("bruteforce: incr %q: %w", key, err)
	}
	if err := g.rc.Expire(ctx, key, LockoutDuration).Err(); err != nil {
		return fmt.Errorf("bruteforce: expire %q: %w", key, err)
	}
	return nil
}

// Reset clears the failure counter after a successful authentication.
func (g *BruteForceGuard) Reset(ctx context.Context, username string) error {
	if g == nil || g.rc == nil {
		return nil
	}
	if err := g.rc.Del(ctx, BruteForceKey(username)).Err(); err != nil {
		return fmt.Errorf("bruteforce: reset %q: %w", BruteForceKey(username), err)
	}
	return nil
}
