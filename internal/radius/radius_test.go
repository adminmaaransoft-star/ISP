// Unit tests for the RADIUS package's pure helpers and the brute-force guard.
//
// Every test in this file previously passed without calling the code it named.
// TestBruteForceKeyFormat compared a string literal to itself; the two
// TestRateLimitSelection_* tests re-implemented the FUP branch inline in the
// test body instead of calling RateLimitForSubscriber; TestDedupKey built its
// key by concatenation and discarded the input it claimed to use via
// `_ = inputOctets`. One even carried the comment "suppress unused import in
// stub file", which is what they were. They reported the package as tested
// while RateLimitForSubscriber and IsBlocked sat at 0% coverage.
//
// They are replaced rather than added to. Dedup key construction is not
// re-tested here because it has no exported function to call — it is built
// inline in handleAccounting and is already covered end to end by
// TestFR_AAA_003_Dedup_DuplicateInterimSkipped in integration_test.go.
package radius_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

func newGuard(t *testing.T) (*radius.BruteForceGuard, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return radius.NewBruteForceGuard(rc), mr
}

func TestFR_SEC_001_BruteForceKeyFormat(t *testing.T) {
	if got := radius.BruteForceKey("testuser"); got != "bf_attempts:testuser" {
		t.Errorf("BruteForceKey: want bf_attempts:testuser, got %q", got)
	}
}

// TestFR_AAA_004_RateLimitForSubscriber covers every branch of the effective
// rate-limit decision, including the one the old tests missed entirely:
// FUPActive with an empty throttle string must fall back to the plan rate
// rather than return "".
func TestFR_AAA_004_RateLimitForSubscriber(t *testing.T) {
	cases := []struct {
		name string
		sub  radius.Subscriber
		want string
	}{
		{
			name: "FUP inactive uses the plan rate",
			sub:  radius.Subscriber{RateLimitStr: "100M/100M", FUPActive: false, FUPThrottle: "10M/10M"},
			want: "100M/100M",
		},
		{
			name: "FUP active uses the throttle",
			sub:  radius.Subscriber{RateLimitStr: "100M/100M", FUPActive: true, FUPThrottle: "10M/10M"},
			want: "10M/10M",
		},
		{
			name: "FUP active with an empty throttle falls back to the plan rate",
			sub:  radius.Subscriber{RateLimitStr: "100M/100M", FUPActive: true, FUPThrottle: ""},
			want: "100M/100M",
		},
		{
			name: "FUP inactive with an empty plan rate returns empty",
			sub:  radius.Subscriber{RateLimitStr: "", FUPActive: false, FUPThrottle: "10M/10M"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := radius.RateLimitForSubscriber(&tc.sub); got != tc.want {
				t.Errorf("RateLimitForSubscriber: want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestFR_SEC_001_BruteForceGuard_IsBlockedAtThreshold pins the boundary: the
// guard must block at exactly MaxFailedAttempts, not one either side.
func TestFR_SEC_001_BruteForceGuard_IsBlockedAtThreshold(t *testing.T) {
	ctx := context.Background()

	t.Run("below the threshold is not blocked", func(t *testing.T) {
		g, mr := newGuard(t)
		if err := mr.Set(radius.BruteForceKey("u"), strconv.Itoa(radius.MaxFailedAttempts-1)); err != nil {
			t.Fatalf("seed counter: %v", err)
		}
		blocked, err := g.IsBlocked(ctx, "u")
		if err != nil {
			t.Fatalf("IsBlocked: %v", err)
		}
		if blocked {
			t.Errorf("must not block at %d failures", radius.MaxFailedAttempts-1)
		}
	})

	t.Run("at the threshold is blocked", func(t *testing.T) {
		g, mr := newGuard(t)
		if err := mr.Set(radius.BruteForceKey("u"), strconv.Itoa(radius.MaxFailedAttempts)); err != nil {
			t.Fatalf("seed counter: %v", err)
		}
		blocked, err := g.IsBlocked(ctx, "u")
		if err != nil {
			t.Fatalf("IsBlocked: %v", err)
		}
		if !blocked {
			t.Errorf("must block at %d failures", radius.MaxFailedAttempts)
		}
	})
}

func TestFR_SEC_001_BruteForceGuard_UnknownUserNotBlocked(t *testing.T) {
	g, _ := newGuard(t)
	blocked, err := g.IsBlocked(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("IsBlocked: %v", err)
	}
	if blocked {
		t.Error("a username with no counter must not be blocked")
	}
}

// TestFR_SEC_001_BruteForceGuard_CheckReportsHasFailures covers the second
// return value, which exists purely to let handleAuth skip a Redis DELETE on
// the hot path when there is nothing to reset.
func TestFR_SEC_001_BruteForceGuard_CheckReportsHasFailures(t *testing.T) {
	ctx := context.Background()
	g, _ := newGuard(t)

	_, hasFailures, err := g.Check(ctx, "u")
	if err != nil {
		t.Fatalf("Check on a clean user: %v", err)
	}
	if hasFailures {
		t.Error("a user with no counter must report hasFailures=false")
	}

	if err := g.RecordFailure(ctx, "u"); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	blocked, hasFailures, err := g.Check(ctx, "u")
	if err != nil {
		t.Fatalf("Check after one failure: %v", err)
	}
	if !hasFailures {
		t.Error("after a recorded failure, hasFailures must be true")
	}
	if blocked {
		t.Error("one failure must not block")
	}
}

// TestFR_SEC_001_BruteForceGuard_CorruptCounterDoesNotLockOut covers the
// deliberate choice in Check: a non-numeric counter must not lock a subscriber
// out permanently, but must still be reported as resettable so the next
// success clears it.
func TestFR_SEC_001_BruteForceGuard_CorruptCounterDoesNotLockOut(t *testing.T) {
	g, mr := newGuard(t)
	if err := mr.Set(radius.BruteForceKey("u"), "not-a-number"); err != nil {
		t.Fatalf("seed corrupt counter: %v", err)
	}

	blocked, hasFailures, err := g.Check(context.Background(), "u")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if blocked {
		t.Error("a corrupt counter must not lock a subscriber out")
	}
	if !hasFailures {
		t.Error("a corrupt counter must still be reported so it gets cleared on success")
	}
}

// TestFR_SEC_001_BruteForceGuard_RecordFailureSetsLockoutTTL verifies the
// counter is given the lockout TTL. Without it the key would live forever and
// a subscriber who failed ten times months apart would be locked out.
func TestFR_SEC_001_BruteForceGuard_RecordFailureSetsLockoutTTL(t *testing.T) {
	g, mr := newGuard(t)
	if err := g.RecordFailure(context.Background(), "u"); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	ttl := mr.TTL(radius.BruteForceKey("u"))
	if ttl <= 0 {
		t.Fatal("the failure counter must carry a TTL")
	}
	if ttl > radius.LockoutDuration {
		t.Errorf("TTL %s exceeds LockoutDuration %s", ttl, radius.LockoutDuration)
	}
}

// TestFR_SEC_001_BruteForceGuard_TTLRefreshedOnEachFailure — the lockout window
// must slide forward with continued attacks rather than expire while one is in
// progress.
func TestFR_SEC_001_BruteForceGuard_TTLRefreshedOnEachFailure(t *testing.T) {
	ctx := context.Background()
	g, mr := newGuard(t)

	if err := g.RecordFailure(ctx, "u"); err != nil {
		t.Fatalf("first failure: %v", err)
	}
	mr.FastForward(5 * time.Minute)
	if err := g.RecordFailure(ctx, "u"); err != nil {
		t.Fatalf("second failure: %v", err)
	}

	if ttl := mr.TTL(radius.BruteForceKey("u")); ttl <= radius.LockoutDuration-5*time.Minute {
		t.Errorf("TTL should have been refreshed to the full lockout window, got %s", ttl)
	}
}

func TestFR_SEC_001_BruteForceGuard_ResetClearsCounter(t *testing.T) {
	ctx := context.Background()
	g, mr := newGuard(t)

	for i := 0; i < radius.MaxFailedAttempts; i++ {
		if err := g.RecordFailure(ctx, "u"); err != nil {
			t.Fatalf("RecordFailure %d: %v", i, err)
		}
	}
	blocked, err := g.IsBlocked(ctx, "u")
	if err != nil || !blocked {
		t.Fatalf("want blocked before reset (blocked=%v err=%v)", blocked, err)
	}

	if err := g.Reset(ctx, "u"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if mr.Exists(radius.BruteForceKey("u")) {
		t.Error("Reset must delete the counter key")
	}
	blocked, err = g.IsBlocked(ctx, "u")
	if err != nil {
		t.Fatalf("IsBlocked after reset: %v", err)
	}
	if blocked {
		t.Error("a subscriber must be able to authenticate again after a reset")
	}
}

// TestFR_SEC_001_BruteForceGuard_NilGuardIsInert covers the nil-receiver and
// nil-client guards. The daemon runs without Redis in some configurations, and
// in that mode the guard must degrade to "never blocks" rather than panic on
// the authentication path.
func TestFR_SEC_001_BruteForceGuard_NilGuardIsInert(t *testing.T) {
	ctx := context.Background()

	for name, g := range map[string]*radius.BruteForceGuard{
		"nil guard":        nil,
		"nil redis client": radius.NewBruteForceGuard(nil),
	} {
		t.Run(name, func(t *testing.T) {
			blocked, hasFailures, err := g.Check(ctx, "u")
			if err != nil || blocked || hasFailures {
				t.Errorf("Check: want (false,false,nil), got (%v,%v,%v)", blocked, hasFailures, err)
			}
			if err := g.RecordFailure(ctx, "u"); err != nil {
				t.Errorf("RecordFailure: want nil, got %v", err)
			}
			if err := g.Reset(ctx, "u"); err != nil {
				t.Errorf("Reset: want nil, got %v", err)
			}
			blocked, err = g.IsBlocked(ctx, "u")
			if err != nil || blocked {
				t.Errorf("IsBlocked: want (false,nil), got (%v,%v)", blocked, err)
			}
		})
	}
}

// TestFR_SEC_001_BruteForceGuard_RedisDownSurfacesError — unlike the subscriber
// cache, a brute-force read failure is reported rather than swallowed: silently
// treating it as "not blocked" would disable the lockout whenever Redis blips.
func TestFR_SEC_001_BruteForceGuard_RedisDownSurfacesError(t *testing.T) {
	g, mr := newGuard(t)
	mr.Close()

	if _, err := g.IsBlocked(context.Background(), "u"); err == nil {
		t.Error("a Redis failure must surface rather than report 'not blocked'")
	}
	if err := g.RecordFailure(context.Background(), "u"); err == nil {
		t.Error("RecordFailure must surface a Redis failure")
	}
	if err := g.Reset(context.Background(), "u"); err == nil {
		t.Error("Reset must surface a Redis failure")
	}
}
