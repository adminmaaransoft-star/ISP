package db

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// testHAPool builds a haPool with no live database, only a fake resetFunc
// — enough to exercise checkFailover's detection/debounce logic in
// isolation, per hapool.go's resetFunc field comment.
func testHAPool() (*haPool, *atomic.Int32) {
	var resetCalls atomic.Int32
	p := &haPool{resetFunc: func() { resetCalls.Add(1) }}
	return p, &resetCalls
}

func TestCheckFailover_ReadOnlySQLState_TriggersReset(t *testing.T) {
	p, resetCalls := testHAPool()

	p.checkFailover(&pgconn.PgError{Code: readOnlyTxnSQLState, Message: "cannot execute INSERT in a read-only transaction"})

	if got := resetCalls.Load(); got != 1 {
		t.Errorf("resetFunc called %d times, want 1", got)
	}
}

func TestCheckFailover_OtherPgErrors_DoNotTriggerReset(t *testing.T) {
	p, resetCalls := testHAPool()

	// A handful of real, unrelated SQLSTATEs that must never trigger a pool
	// reset — a unique-constraint violation or a syntax error is not
	// evidence of a demoted primary.
	codes := []string{"23505", "42601", "08006", "40001"}
	for _, code := range codes {
		p.checkFailover(&pgconn.PgError{Code: code})
	}

	if got := resetCalls.Load(); got != 0 {
		t.Errorf("resetFunc called %d times for non-25006 errors, want 0", got)
	}
}

func TestCheckFailover_NilOrNonPgError_DoesNotTriggerReset(t *testing.T) {
	p, resetCalls := testHAPool()

	p.checkFailover(nil)
	p.checkFailover(errors.New("some ordinary error"))
	p.checkFailover(context.DeadlineExceeded)

	if got := resetCalls.Load(); got != 0 {
		t.Errorf("resetFunc called %d times, want 0", got)
	}
}

func TestCheckFailover_WrappedPgError_StillDetected(t *testing.T) {
	// db.go's stores consistently wrap driver errors with fmt.Errorf("...: %w", err)
	// — checkFailover must see through that via errors.As, the same way
	// every other error-classification in this codebase already does
	// (see isNoRows in db.go).
	p, resetCalls := testHAPool()

	wrapped := fmt.Errorf("db: exec failed: %w", &pgconn.PgError{Code: readOnlyTxnSQLState})
	p.checkFailover(wrapped)

	if got := resetCalls.Load(); got != 1 {
		t.Errorf("resetFunc called %d times for a wrapped 25006, want 1", got)
	}
}

func TestCheckFailover_Debounces_ConcurrentBurst(t *testing.T) {
	// The scenario this test protects: a real failover fails many
	// concurrent writers in the same instant. Exactly one Reset() should
	// happen, not fifty.
	p, resetCalls := testHAPool()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			p.checkFailover(&pgconn.PgError{Code: readOnlyTxnSQLState})
		}()
	}
	wg.Wait()

	if got := resetCalls.Load(); got != 1 {
		t.Errorf("resetFunc called %d times for a %d-goroutine burst, want exactly 1", got, goroutines)
	}
}

func TestCheckFailover_ResetsAgainAfterDebounceWindow(t *testing.T) {
	p, resetCalls := testHAPool()

	p.checkFailover(&pgconn.PgError{Code: readOnlyTxnSQLState})
	if got := resetCalls.Load(); got != 1 {
		t.Fatalf("first call: resetFunc called %d times, want 1", got)
	}

	// Simulate the debounce window having already elapsed rather than
	// sleeping resetDebounce (2s) in a unit test.
	p.lastReset.Store(time.Now().Add(-resetDebounce - time.Second).UnixNano())

	p.checkFailover(&pgconn.PgError{Code: readOnlyTxnSQLState})
	if got := resetCalls.Load(); got != 2 {
		t.Errorf("after the debounce window elapsed: resetFunc called %d times total, want 2", got)
	}
}
