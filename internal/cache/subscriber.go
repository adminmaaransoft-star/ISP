package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

var (
	subscriberCacheHits = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_subscriber_cache_hits_total",
		Help: "Authentication lookups served from Redis",
	})
	subscriberCacheMisses = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_subscriber_cache_misses_total",
		Help: "Authentication lookups that fell through to PostgreSQL",
	})
	subscriberCacheErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "radius_subscriber_cache_errors_total",
		Help: "Redis errors on the authentication path (served from PostgreSQL instead)",
	})
)

// DefaultSubscriberTTL bounds how long a cached authentication record is served.
//
// This is the window in which a subscriber suspended in the database could still
// re-authenticate. It is kept short deliberately, and callers that change
// auth-relevant state should call InvalidateSubscriber rather than rely on it.
// Suspension also issues a Disconnect-Request, so the TTL is a backstop for
// re-authentication, not the primary enforcement path.
const DefaultSubscriberTTL = 60 * time.Second

// SubscriberCacheKey returns the Redis key holding a cached auth record.
func SubscriberCacheKey(username string) string {
	return "subscriber:auth:" + username
}

// cachedSubscriber is the stored form. A separate type from radius.Subscriber so
// the wire format is explicit and does not silently change when a field is added
// to the domain struct.
type cachedSubscriber struct {
	ID           int    `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Status       string `json:"status"`
	RateLimitStr string `json:"rate_limit"`
	FUPActive    bool   `json:"fup_active"`
	FUPThrottle  string `json:"fup_throttle"`
	// NotFound records a negative result, so a flood of requests for a username
	// that does not exist cannot turn into a flood of database queries.
	NotFound bool `json:"not_found,omitempty"`
}

// SubscriberCache is a read-through Redis cache in front of the authentication
// lookup, implementing SAD's "RADIUS never touches PostgreSQL on the hot path"
// and the ≤5ms budget FR-AAA-002 sets.
//
// It satisfies radius.DBQuerier, so the daemon is unaware it is cached.
//
// Redis is treated as an optimisation, never a dependency: any Redis failure
// falls through to PostgreSQL and authentication continues to work.
type SubscriberCache struct {
	db  radius.DBQuerier
	rc  redis.UniversalClient
	ttl time.Duration
}

var _ radius.DBQuerier = (*SubscriberCache)(nil)

// NewSubscriberCache wraps db with a Redis read-through cache.
func NewSubscriberCache(db radius.DBQuerier, rc redis.UniversalClient, ttl time.Duration) *SubscriberCache {
	if ttl <= 0 {
		ttl = DefaultSubscriberTTL
	}
	return &SubscriberCache{db: db, rc: rc, ttl: ttl}
}

// GetSubscriberByUsername serves from Redis when possible, falling back to
// PostgreSQL and populating the cache on a miss.
func (c *SubscriberCache) GetSubscriberByUsername(ctx context.Context, username string) (*radius.Subscriber, error) {
	key := SubscriberCacheKey(username)

	raw, err := c.rc.Get(ctx, key).Bytes()
	switch {
	case err == nil:
		var entry cachedSubscriber
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr == nil {
			subscriberCacheHits.Inc()
			if entry.NotFound {
				return nil, nil
			}
			return &radius.Subscriber{
				ID:           entry.ID,
				Username:     entry.Username,
				PasswordHash: entry.PasswordHash,
				Status:       entry.Status,
				RateLimitStr: entry.RateLimitStr,
				FUPActive:    entry.FUPActive,
				FUPThrottle:  entry.FUPThrottle,
			}, nil
		}
		// A corrupt entry is treated as a miss rather than an error: the
		// database still holds the truth.
		subscriberCacheErrors.Inc()
	case errors.Is(err, redis.Nil):
		subscriberCacheMisses.Inc()
	default:
		// Redis is down or slow. Authentication must not fail with it.
		subscriberCacheErrors.Inc()
	}

	sub, dbErr := c.db.GetSubscriberByUsername(ctx, username)
	if dbErr != nil {
		return nil, dbErr
	}

	c.store(ctx, key, sub, username)
	return sub, nil
}

// store writes the lookup result, including a negative entry for an unknown
// username. Cache write failures are silent: the caller already has its answer.
func (c *SubscriberCache) store(ctx context.Context, key string, sub *radius.Subscriber, username string) {
	entry := cachedSubscriber{Username: username, NotFound: true}
	if sub != nil {
		entry = cachedSubscriber{
			ID:           sub.ID,
			Username:     sub.Username,
			PasswordHash: sub.PasswordHash,
			Status:       sub.Status,
			RateLimitStr: sub.RateLimitStr,
			FUPActive:    sub.FUPActive,
			FUPThrottle:  sub.FUPThrottle,
		}
	}

	payload, err := json.Marshal(entry)
	if err != nil {
		subscriberCacheErrors.Inc()
		return
	}

	ttl := c.ttl
	if entry.NotFound {
		// Negative entries expire faster: a newly provisioned subscriber should
		// not be locked out for a full TTL by a lookup that preceded them.
		ttl = c.ttl / 4
		if ttl < time.Second {
			ttl = time.Second
		}
	}

	if err := c.rc.Set(ctx, key, payload, ttl).Err(); err != nil {
		subscriberCacheErrors.Inc()
	}
}

// InvalidateSubscriber drops a cached record so the next authentication reloads
// it. Call this whenever status, plan or FUP state changes: without it, a
// suspension takes up to one TTL to reach the authentication path.
func (c *SubscriberCache) InvalidateSubscriber(ctx context.Context, username string) error {
	if err := c.rc.Del(ctx, SubscriberCacheKey(username)).Err(); err != nil {
		return fmt.Errorf("cache: invalidate subscriber %q: %w", username, err)
	}
	return nil
}
