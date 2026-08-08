package radius

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/bcrypt"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

// handleRequest dispatches Access-Request or Accounting-Request packets.
//
// parent is the daemon lifetime, so a shutdown cancels in-flight backend calls
// rather than leaving workers blocked on Redis or PostgreSQL.
func (d *RadiusDaemon) handleRequest(parent context.Context, w radius.ResponseWriter, r *radius.Request) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()

	switch r.Code {
	case radius.CodeAccessRequest:
		d.handleAuth(ctx, w, r)
	case radius.CodeAccountingRequest:
		d.handleAccounting(ctx, w, r)
	default:
		// Ignore unknown packet types
	}
}

// handleAuth validates username/password and checks subscriber status.
//
// FR: FR-AAA-001..002 | DDS Ã‚Â§5.1
func (d *RadiusDaemon) handleAuth(ctx context.Context, w radius.ResponseWriter, r *radius.Request) {
	// Timed here rather than in the worker loop so radius_auth_duration_seconds
	// measures only Access-Request handling, as its name promises.
	timer := prometheus.NewTimer(radiusAuthDuration)
	defer timer.ObserveDuration()

	username := rfc2865.UserName_GetString(r.Packet)
	password := rfc2865.UserPassword_GetString(r.Packet)

	// Brute-force lockout is checked before any credential work so that a banned
	// username costs neither a DB round-trip nor a bcrypt comparison.
	blocked, hasFailures, err := d.guard.Check(ctx, username)
	if err != nil {
		log.Error().Err(err).Str("username", username).Msg("radius: brute-force check failed")
	} else if blocked {
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		radiusAuthReject.Inc()
		return
	}

	sub, err := d.db.GetSubscriberByUsername(ctx, username)
	if err != nil || sub == nil {
		d.recordAuthFailure(ctx, username)
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		radiusAuthReject.Inc()
		return
	}

	// Reject immediately for hard-suspended / terminated subscribers
	if sub.Status == "hard_suspended" || sub.Status == "terminated" {
		w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
		radiusAuthReject.Inc()
		return
	}

	// Fast path: skip bcrypt cost=12 (~280ms, ~19x the 15ms p99 budget) if this
	// exact password was already bcrypt-verified against this exact password
	// hash recently. Binding to sub.PasswordHash (not just the password) means
	// a password change self-invalidates immediately: the old password no
	// longer matches the cached verifier once the hash changes, even within
	// the cache's TTL. A miss or mismatch here is NOT a rejection — it only
	// means "pay the full bcrypt cost below", same as if the cache did not
	// exist.
	authenticated, err := d.verifierCache.Check(ctx, username, password, sub.PasswordHash)
	if err != nil {
		log.Warn().Err(err).Str("username", username).Msg("radius: verifier cache check failed")
	}

	if !authenticated {
		// bcrypt password check (cost=12 per spec) — the authoritative check.
		if err := bcrypt.CompareHashAndPassword([]byte(sub.PasswordHash), []byte(password)); err != nil {
			d.recordAuthFailure(ctx, username)
			w.Write(r.Response(radius.CodeAccessReject)) //nolint:errcheck,gosec
			radiusAuthReject.Inc()
			return
		}
		if err := d.verifierCache.Store(ctx, username, password, sub.PasswordHash); err != nil {
			log.Warn().Err(err).Str("username", username).Msg("radius: verifier cache store failed")
		}
	}

	// Successful auth clears the failure counter so a later typo does not inherit
	// attempts from an old burst. Skipped when the Check above found no counter,
	// which is the common case and saves a Redis round-trip on the hot path.
	if hasFailures {
		if err := d.guard.Reset(ctx, username); err != nil {
			log.Warn().Err(err).Str("username", username).Msg("radius: brute-force reset failed")
		}
	}

	// Build Accept response with rate-limit reply attributes (MikroTik-Rate-Limit)
	resp := r.Response(radius.CodeAccessAccept)
	rateLimit := sub.RateLimitStr
	if sub.FUPActive && sub.FUPThrottle != "" {
		rateLimit = sub.FUPThrottle
	}
	// Vendor-specific attribute: MikroTik-Rate-Limit (vendor 14988, attr 8)
	// VSA vendor-data format: vendor-type(1) + vendor-length(1) + value(N)
	rlBytes := []byte(rateLimit)
	vsaData := make([]byte, 2+len(rlBytes))
	vsaData[0] = 8             // MikroTik-Rate-Limit vendor attribute type
	if 2+len(rlBytes) <= 255 { // RADIUS attribute value max 253 bytes
		vsaData[1] = byte(2 + len(rlBytes)) //nolint:gosec
	} else {
		vsaData[1] = 255
	}
	copy(vsaData[2:], rlBytes)
	if vsAttr, err := radius.NewVendorSpecific(14988, radius.Attribute(vsaData)); err == nil {
		resp.Add(26, vsAttr) // 26 = Vendor-Specific RADIUS attribute type
	}

	w.Write(resp) //nolint:errcheck,gosec
	radiusAuthAccept.Inc()
}

// handleAccounting processes RADIUS Accounting-Request with deduplication.
//
// FR: FR-AAA-003 | DDS Ã‚Â§5.2
func (d *RadiusDaemon) handleAccounting(ctx context.Context, w radius.ResponseWriter, r *radius.Request) {
	acctSessionID := rfc2865.NASIdentifier_GetString(r.Packet) // reuse as session key
	inputOctets := uint64(0)
	// Extract Acct-Input-Octets if present
	if v := r.Get(radius.Type(42)); v != nil {
		if len(v) == 4 {
			inputOctets = uint64(v[0])<<24 | uint64(v[1])<<16 | uint64(v[2])<<8 | uint64(v[3])
		}
	}

	dedupKey := "acct_dedup:" + acctSessionID + ":" + strconv.FormatUint(inputOctets, 10)
	isNew, err := d.redisClient.SetNX(ctx, dedupKey, "1", 30*time.Second).Result()
	if err != nil || !isNew {
		w.Write(r.Response(radius.CodeAccountingResponse)) //nolint:errcheck,gosec
		radiusDedupSkipped.Inc()
		return
	}

	w.Write(r.Response(radius.CodeAccountingResponse)) //nolint:errcheck,gosec
}

// recordAuthFailure increments the brute-force counter for a rejected credential.
func (d *RadiusDaemon) recordAuthFailure(ctx context.Context, username string) {
	if err := d.guard.RecordFailure(ctx, username); err != nil {
		log.Error().Err(err).Str("username", username).Msg("radius: brute-force record failed")
	}
}

// RateLimitForSubscriber returns the effective rate-limit string (respects FUP).
func RateLimitForSubscriber(sub *Subscriber) string {
	if sub.FUPActive && sub.FUPThrottle != "" {
		return sub.FUPThrottle
	}
	return sub.RateLimitStr
}

// BruteForceKey returns the Redis key used for brute-force rate limiting.
func BruteForceKey(username string) string {
	return fmt.Sprintf("bf_attempts:%s", username)
}
