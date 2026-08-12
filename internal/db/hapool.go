package db

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// dbPool is the subset of *pgxpool.Pool this package's stores actually
// call (confirmed by grep across internal/db: Exec, Query, QueryRow, and
// Begin for the two inTx callers in billing.go and revenue.go — nothing
// else). Every store struct holds this interface, not the concrete
// *pgxpool.Pool, so haPool can sit between them without touching call sites.
type dbPool interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

var _ dbPool = (*pgxpool.Pool)(nil)

// readOnlyTxnSQLState is Postgres error code 25006
// (read_only_sql_transaction): a write was attempted against a connection
// whose target has become a read-only standby. IDD §8.2a / MDS §4.12: the
// multi-host DSN (target_session_attrs=read-write, verified against pgx's
// own ParseConfig to match libpq's behavior) makes every *new* connection
// resolve to the current primary automatically after a Patroni failover.
// It does nothing for a connection pgxpool already had checked out at the
// moment of failover — that one is still physically talking to the old
// primary, now a standby. 25006 is a normal SQL error, not a broken
// connection, so pgxpool has no built-in reason to evict it; without this
// file, that connection keeps being handed out of the pool and keeps
// failing every write until it happens to be recycled by MaxConnLifetime.
const readOnlyTxnSQLState = "25006"

// resetDebounce collapses a burst of concurrent requests that all hit
// SQLSTATE 25006 in the same failover window into a single Pool.Reset()
// call. Without it, fifty concurrent writers failing in the same instant
// would each independently reset a pool a moment-earlier goroutine had
// already reset.
const resetDebounce = 2 * time.Second

// haPool wraps *pgxpool.Pool to detect SQLSTATE 25006 and call Reset() —
// "intended for use when an error is detected that would disrupt all
// connections (such as a network interruption or a server state change)"
// per pgxpool's own doc comment on Reset, which is exactly a Patroni
// failover. Reset closes idle connections immediately and marks checked-out
// ones for closure when they are next released, rather than reused.
//
// Embeds *pgxpool.Pool so every method not explicitly overridden (Close,
// Ping, Stat, ...) passes through unchanged.
type haPool struct {
	*pgxpool.Pool
	lastReset atomic.Int64 // UnixNano of the last Reset() this wrapper triggered
	// resetFunc defaults to Pool.Reset; overridable in tests so the
	// detection/debounce logic can be exercised without a live database.
	resetFunc func()
}

// newHAPool wraps pool.
func newHAPool(pool *pgxpool.Pool) *haPool {
	p := &haPool{Pool: pool}
	p.resetFunc = p.Reset
	return p
}

// checkFailover resets the pool at most once per resetDebounce window when
// err is SQLSTATE 25006, otherwise does nothing. A CompareAndSwap, not a
// mutex: many goroutines can hit this in the same instant during a real
// failover, and exactly one of them should win the race to actually call
// Reset — the rest recognize a reset just happened and skip it, since it
// already covers them too.
func (p *haPool) checkFailover(err error) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != readOnlyTxnSQLState {
		return
	}

	now := time.Now().UnixNano()
	last := p.lastReset.Load()
	if now-last < int64(resetDebounce) {
		return
	}
	if !p.lastReset.CompareAndSwap(last, now) {
		return // lost the race to another goroutine; its Reset() covers this one too
	}

	log.Warn().Msg("db: SQLSTATE 25006 (read_only_sql_transaction) — a pooled connection was talking to a demoted primary; resetting the pool so new connections re-resolve the current primary (IDD §8.2a)")
	p.resetFunc()
}

func (p *haPool) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	tag, err := p.Pool.Exec(ctx, sql, arguments...)
	p.checkFailover(err)
	return tag, err
}

func (p *haPool) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := p.Pool.Query(ctx, sql, args...)
	p.checkFailover(err)
	return rows, err
}

// QueryRow never returns an error directly in the pgx API — it surfaces
// only when the caller calls Scan on the row handed back, so the check has
// to live there instead.
func (p *haPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &haRow{Row: p.Pool.QueryRow(ctx, sql, args...), pool: p}
}

func (p *haPool) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.Pool.Begin(ctx)
	p.checkFailover(err)
	if err != nil {
		return nil, err
	}
	return &haTx{Tx: tx, pool: p}, nil
}

type haRow struct {
	pgx.Row
	pool *haPool
}

func (r *haRow) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.pool.checkFailover(err)
	return err
}

// haTx wraps a transaction so a write inside it is checked the same way a
// pool-level Exec/QueryRow is. Only Exec, QueryRow and Commit are
// overridden — Query, CopyFrom, SendBatch, nested Begin, etc. pass through
// unwrapped because nothing in this codebase calls them inside a
// transaction today (internal/db/billing.go and internal/db/revenue.go,
// the only two inTx callers, use only Exec and QueryRow).
type haTx struct {
	pgx.Tx
	pool *haPool
}

func (t *haTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	tag, err := t.Tx.Exec(ctx, sql, arguments...)
	t.pool.checkFailover(err)
	return tag, err
}

func (t *haTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &haRow{Row: t.Tx.QueryRow(ctx, sql, args...), pool: t.pool}
}

func (t *haTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.pool.checkFailover(err)
	return err
}
