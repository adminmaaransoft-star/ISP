//go:build integration

// Integration tests for the RADIUS AAA module.
//
// Covers INT-AAA-001 .. INT-AAA-005 from the Integration Tests tracker sheet.
// Redis is a real server (miniredis, in-process) rather than a mock, so key
// formats, TTLs and SetNX semantics are exercised for real.
//
// Run: ./scripts/run_tests.ps1 -Pkg ./internal/radius -Tags integration
package radius

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"layeh.com/radius"
	"layeh.com/radius/rfc2865"
)

var itSecret = []byte("testing123")

// ── Test doubles ────────────────────────────────────────────────────────────

// itResponseWriter captures the packets a handler writes.
type itResponseWriter struct {
	mu      sync.Mutex
	packets []*radius.Packet
}

func (w *itResponseWriter) Write(p *radius.Packet) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.packets = append(w.packets, p)
	return nil
}

func (w *itResponseWriter) last() *radius.Packet {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.packets) == 0 {
		return nil
	}
	return w.packets[len(w.packets)-1]
}

// itSubscriberDB is an in-memory DBQuerier.
type itSubscriberDB struct {
	subs map[string]*Subscriber
}

func (db *itSubscriberDB) GetSubscriberByUsername(_ context.Context, username string) (*Subscriber, error) {
	sub, ok := db.subs[username]
	if !ok {
		return nil, nil
	}
	return sub, nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func itNewDaemon(t *testing.T, subs map[string]*Subscriber) (*RadiusDaemon, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return NewRadiusDaemon(":0", itSecret, &itSubscriberDB{subs: subs}, rc), mr
}

func itHashPassword(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt hash: %v", err)
	}
	return string(h)
}

func itAccessRequest(t *testing.T, username, password string) *radius.Request {
	t.Helper()
	pkt := radius.New(radius.CodeAccessRequest, itSecret)
	if err := rfc2865.UserName_SetString(pkt, username); err != nil {
		t.Fatalf("set User-Name: %v", err)
	}
	if err := rfc2865.UserPassword_SetString(pkt, password); err != nil {
		t.Fatalf("set User-Password: %v", err)
	}
	return &radius.Request{Packet: pkt}
}

func itAccountingRequest(t *testing.T, sessionID string, inputOctets uint32) *radius.Request {
	t.Helper()
	pkt := radius.New(radius.CodeAccountingRequest, itSecret)
	if err := rfc2865.NASIdentifier_SetString(pkt, sessionID); err != nil {
		t.Fatalf("set NAS-Identifier: %v", err)
	}
	octets := []byte{
		byte(inputOctets >> 24), byte(inputOctets >> 16),
		byte(inputOctets >> 8), byte(inputOctets),
	}
	pkt.Add(radius.Type(42), radius.Attribute(octets)) // Acct-Input-Octets
	return &radius.Request{Packet: pkt}
}

// itCounterValue reads the current value of a counter for delta assertions.
func itCounterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

// itHistogramCount reads the observation count of a histogram.
func itHistogramCount(t *testing.T, h prometheus.Histogram) uint64 {
	t.Helper()
	var m dto.Metric
	collector, ok := h.(prometheus.Metric)
	if !ok {
		t.Fatalf("histogram does not implement prometheus.Metric")
	}
	if err := collector.Write(&m); err != nil {
		t.Fatalf("read histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// ── INT-AAA-001 ─────────────────────────────────────────────────────────────

// TestHandleAuth_ActiveSubscriberAccepted verifies an active subscriber with
// correct credentials receives Access-Accept and that the latency histogram
// records the request.
//
// INT-AAA-001 | FR-AAA-002
func TestHandleAuth_ActiveSubscriberAccepted(t *testing.T) {
	d, _ := itNewDaemon(t, map[string]*Subscriber{
		"alice@isp": {
			ID:           1,
			Username:     "alice@isp",
			PasswordHash: itHashPassword(t, "correct-horse"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})

	beforeAccept := itCounterValue(t, radiusAuthAccept)
	beforeLatency := itHistogramCount(t, radiusAuthDuration)

	w := &itResponseWriter{}
	d.handleAuth(context.Background(), w, itAccessRequest(t, "alice@isp", "correct-horse"))

	resp := w.last()
	if resp == nil {
		t.Fatal("handler wrote no response packet")
	}
	if resp.Code != radius.CodeAccessAccept {
		t.Errorf("want Access-Accept, got %v", resp.Code)
	}
	if got := itCounterValue(t, radiusAuthAccept); got != beforeAccept+1 {
		t.Errorf("radius_auth_accept_total: want +1, got %v", got-beforeAccept)
	}
	if got := itHistogramCount(t, radiusAuthDuration); got != beforeLatency+1 {
		t.Errorf("latency metric not emitted: sample count went %d -> %d", beforeLatency, got)
	}

	// The Accept must carry the subscriber's rate limit as a MikroTik VSA.
	if vsa := resp.Get(radius.Type(26)); vsa == nil {
		t.Error("expected Vendor-Specific rate-limit attribute on Access-Accept")
	}
}

// ── INT-AAA-002 ─────────────────────────────────────────────────────────────

// TestHandleAuth_InvalidPassword verifies a wrong password yields Access-Reject.
//
// INT-AAA-002 | FR-AAA-002
func TestHandleAuth_InvalidPassword(t *testing.T) {
	d, _ := itNewDaemon(t, map[string]*Subscriber{
		"bob@isp": {
			ID:           2,
			Username:     "bob@isp",
			PasswordHash: itHashPassword(t, "right-password"),
			Status:       "active",
			RateLimitStr: "50M/50M",
		},
	})

	beforeReject := itCounterValue(t, radiusAuthReject)

	w := &itResponseWriter{}
	d.handleAuth(context.Background(), w, itAccessRequest(t, "bob@isp", "wrong-password"))

	resp := w.last()
	if resp == nil {
		t.Fatal("handler wrote no response packet")
	}
	if resp.Code != radius.CodeAccessReject {
		t.Errorf("want Access-Reject, got %v", resp.Code)
	}
	if got := itCounterValue(t, radiusAuthReject); got != beforeReject+1 {
		t.Errorf("radius_auth_reject_total: want +1, got %v", got-beforeReject)
	}
}

// ── INT-AAA-003 ─────────────────────────────────────────────────────────────

// TestHandleAuth_SuspendedSubscriber verifies hard-suspended and terminated
// subscribers are rejected even with correct credentials, and that no session
// state is cached for them.
//
// INT-AAA-003 | FR-AAA-002
func TestHandleAuth_SuspendedSubscriber(t *testing.T) {
	for _, status := range []string{"hard_suspended", "terminated"} {
		t.Run(status, func(t *testing.T) {
			d, mr := itNewDaemon(t, map[string]*Subscriber{
				"carol@isp": {
					ID:           3,
					Username:     "carol@isp",
					PasswordHash: itHashPassword(t, "valid-pass"),
					Status:       status,
					RateLimitStr: "100M/100M",
				},
			})

			w := &itResponseWriter{}
			d.handleAuth(context.Background(), w, itAccessRequest(t, "carol@isp", "valid-pass"))

			resp := w.last()
			if resp == nil {
				t.Fatal("handler wrote no response packet")
			}
			if resp.Code != radius.CodeAccessReject {
				t.Errorf("status=%s: want Access-Reject, got %v", status, resp.Code)
			}
			// No session may be cached for a rejected subscriber.
			if keys := mr.Keys(); len(keys) != 0 {
				t.Errorf("status=%s: expected no Redis keys after reject, got %v", status, keys)
			}
		})
	}
}

// ── INT-AAA-004 ─────────────────────────────────────────────────────────────

// TestBruteForce_BlocksAt10Failures verifies that after MaxFailedAttempts
// consecutive failures the next attempt is rejected by the guard, and that the
// ban key carries the 15-minute lockout TTL.
//
// INT-AAA-004 | FR-SEC-001
func TestBruteForce_BlocksAt10Failures(t *testing.T) {
	d, mr := itNewDaemon(t, map[string]*Subscriber{
		"dave@isp": {
			ID:           4,
			Username:     "dave@isp",
			PasswordHash: itHashPassword(t, "the-real-password"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	// 10 failed attempts with the wrong password.
	for i := 1; i <= MaxFailedAttempts; i++ {
		w := &itResponseWriter{}
		d.handleAuth(ctx, w, itAccessRequest(t, "dave@isp", "guess"))
		if got := w.last(); got == nil || got.Code != radius.CodeAccessReject {
			t.Fatalf("attempt %d: want Access-Reject, got %v", i, got)
		}
	}

	key := BruteForceKey("dave@isp")
	if !mr.Exists(key) {
		t.Fatalf("expected brute-force counter at key %q", key)
	}
	if got := mr.HGet(key, ""); got != "" { // key must be a string, not a hash
		t.Fatalf("key %q has unexpected hash type", key)
	}
	if got, err := mr.Get(key); err != nil || got != "10" {
		t.Errorf("counter: want \"10\", got %q (err=%v)", got, err)
	}
	if ttl := mr.TTL(key); ttl != LockoutDuration {
		t.Errorf("lockout TTL: want %v, got %v", LockoutDuration, ttl)
	}

	// The 11th attempt is blocked even though the password is now correct.
	beforeBlocked := itCounterValue(t, bruteForceBlocked)
	w := &itResponseWriter{}
	d.handleAuth(ctx, w, itAccessRequest(t, "dave@isp", "the-real-password"))

	resp := w.last()
	if resp == nil {
		t.Fatal("11th attempt wrote no response packet")
	}
	if resp.Code != radius.CodeAccessReject {
		t.Errorf("11th attempt: want Access-Reject while banned, got %v", resp.Code)
	}
	if got := itCounterValue(t, bruteForceBlocked); got != beforeBlocked+1 {
		t.Errorf("radius_bruteforce_blocked_total: want +1, got %v", got-beforeBlocked)
	}

	// Once the lockout expires the correct password works again.
	mr.FastForward(LockoutDuration + time.Second)
	w2 := &itResponseWriter{}
	d.handleAuth(ctx, w2, itAccessRequest(t, "dave@isp", "the-real-password"))
	if got := w2.last(); got == nil || got.Code != radius.CodeAccessAccept {
		t.Errorf("after lockout expiry: want Access-Accept, got %v", got)
	}
}

// TestBruteForce_ResetOnSuccessfulAuth verifies a successful login clears the
// counter so unrelated later typos do not inherit old attempts.
//
// INT-AAA-004 (supporting) | FR-SEC-001
func TestBruteForce_ResetOnSuccessfulAuth(t *testing.T) {
	d, mr := itNewDaemon(t, map[string]*Subscriber{
		"erin@isp": {
			ID:           5,
			Username:     "erin@isp",
			PasswordHash: itHashPassword(t, "s3cret"),
			Status:       "active",
			RateLimitStr: "100M/100M",
		},
	})
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d.handleAuth(ctx, &itResponseWriter{}, itAccessRequest(t, "erin@isp", "nope"))
	}
	if got, _ := mr.Get(BruteForceKey("erin@isp")); got != "3" {
		t.Fatalf("counter before success: want \"3\", got %q", got)
	}

	d.handleAuth(ctx, &itResponseWriter{}, itAccessRequest(t, "erin@isp", "s3cret"))

	if mr.Exists(BruteForceKey("erin@isp")) {
		t.Error("expected brute-force counter to be cleared after successful auth")
	}
}

// ── INT-AAA-005 ─────────────────────────────────────────────────────────────

// TestDedup_DuplicateInterimSkipped verifies a replayed Interim-Update with the
// same session and octet count is counted once only.
//
// Run with -count=3 per the tracker; each run gets a fresh miniredis and the
// assertions are on deltas, so repeats are independent.
//
// INT-AAA-005 | FR-AAA-003
func TestDedup_DuplicateInterimSkipped(t *testing.T) {
	d, mr := itNewDaemon(t, nil)
	ctx := context.Background()

	const sessionID = "sess-abc123"
	const octets = uint32(1234567890)

	beforeSkipped := itCounterValue(t, radiusDedupSkipped)

	// First Interim-Update — accepted and recorded.
	w1 := &itResponseWriter{}
	d.handleAccounting(ctx, w1, itAccountingRequest(t, sessionID, octets))
	if got := w1.last(); got == nil || got.Code != radius.CodeAccountingResponse {
		t.Fatalf("first update: want Accounting-Response, got %v", got)
	}
	if got := itCounterValue(t, radiusDedupSkipped); got != beforeSkipped {
		t.Errorf("first update must not be counted as a duplicate (delta %v)", got-beforeSkipped)
	}

	// Exact replay — acknowledged but not double-counted.
	w2 := &itResponseWriter{}
	d.handleAccounting(ctx, w2, itAccountingRequest(t, sessionID, octets))
	if got := w2.last(); got == nil || got.Code != radius.CodeAccountingResponse {
		t.Fatalf("replay: want Accounting-Response, got %v", got)
	}
	if got := itCounterValue(t, radiusDedupSkipped); got != beforeSkipped+1 {
		t.Errorf("radius_acct_dedup_skipped_total: want +1 on replay, got %v", got-beforeSkipped)
	}

	// Exactly one dedup key exists for this session/octet pair.
	keys := mr.Keys()
	if len(keys) != 1 {
		t.Fatalf("want exactly 1 dedup key, got %d: %v", len(keys), keys)
	}
	wantKey := "acct_dedup:" + sessionID + ":1234567890"
	if keys[0] != wantKey {
		t.Errorf("dedup key: want %q, got %q", wantKey, keys[0])
	}

	// A genuine counter advance (new octet total) is a distinct key, not a dup.
	w3 := &itResponseWriter{}
	d.handleAccounting(ctx, w3, itAccountingRequest(t, sessionID, octets+5000))
	if got := itCounterValue(t, radiusDedupSkipped); got != beforeSkipped+1 {
		t.Errorf("advanced counter must not be deduped (delta %v)", got-beforeSkipped)
	}
	if len(mr.Keys()) != 2 {
		t.Errorf("want 2 dedup keys after counter advance, got %d", len(mr.Keys()))
	}
}
