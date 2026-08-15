package db

import (
	"context"

	"github.com/maaransoft/isp-bss-oss/internal/middleware"
)

// Attribution for the status-capture triggers — FR-RPT-001 | MDS §4.8.
//
// Migration 031 captures every subscriber and ticket status transition with a
// trigger, so the *event* is recorded no matter which code path (or which
// person at a psql prompt) caused it. What a trigger cannot see is who was
// responsible and why, because that lives in the application's request
// context, not in the row.
//
// The bridge is a transaction-local GUC. Each status-writing statement sets
// `app.actor` and `app.change_reason` in the same statement that performs the
// update, and the trigger reads them back with current_setting(..., true).
// Setting them locally means they cannot leak between pooled connections:
// pgx hands the same physical connection to unrelated requests, and a
// session-level setting would misattribute the next one.
//
// Forgetting to set them is not a failure. The transition is still captured,
// with changed_by 'unknown' — losing who made a change is recoverable from
// other logs; losing the fact that it happened is not.

// Each status-writing statement carries attribution in a leading CTE:
//
//	WITH ctx AS (
//	    SELECT set_config('app.actor', $n, true)        AS actor,
//	           set_config('app.change_reason', $m, true) AS reason
//	), upd AS (
//	    UPDATE ... FROM ctx WHERE id = $1 AND ctx.actor IS NOT NULL ...
//	)
//
// A CTE rather than a separate `SET LOCAL` keeps it to one statement, and so
// one implicit transaction, with no explicit BEGIN to leak on an early
// return. The `ctx.actor IS NOT NULL` predicate is always true and exists
// only to *reference* the CTE — an unreferenced CTE is one Postgres is free
// not to evaluate, which would silently drop the attribution.

// actorFromContext resolves the principal to attribute a status change to.
//
// An empty subject means no JWT and no worker annotation — an internal call
// path nobody has labelled yet. It records 'unknown' rather than guessing,
// because a plausible-looking wrong name in an audit trail is worse than an
// honest gap.
func actorFromContext(ctx context.Context) string {
	if sub := middleware.SubjectFromContext(ctx); sub != "" {
		return sub
	}
	return "unknown"
}
