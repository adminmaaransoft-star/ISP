package db

import (
	"context"
	"fmt"
)

// SLA resolution for a new ticket — FR-SUP-001, FR-SUP-003 | MDS §4.13.
//
// Both ticket creation paths (TicketStore.CreateTicketAdmin for staff/API,
// PortalStore.CreateTicket for a subscriber's own ticket) resolve the same
// three things before inserting: what priority applies, which SLA policy
// that priority maps to, and which staff role the ticket routes to. This
// lives in one place rather than in both stores — internal/api and
// internal/staffui already carry near-duplicate copies of the ticket-update
// notification enqueue, and that duplication is not worth extending.

// ticketSLAResolution is what the lookup below produces.
type ticketSLAResolution struct {
	Priority          string
	ResponseMinutes   int
	ResolutionMinutes int
	FranchiseID       *int
	RoutedRole        *string
}

// resolveTicketSLA looks up the priority, SLA window and routing target for
// a ticket about to be created.
//
// overridePriority is non-nil only when a staff caller explicitly set one; a
// subscriber-raised ticket always takes the category default (MDS §4.13 —
// letting the reporter choose their own urgency makes every ticket
// critical).
//
// LEFT JOINs rather than inner: an inner join would collapse "this
// subscriber does not exist", "this category has no default priority" and
// "this (category, priority) pair has no SLA policy" into one indistinguishable
// empty result. They are three different operator problems and each gets its
// own error here.
func resolveTicketSLA(ctx context.Context, pool dbPool, subscriberID int, category string, overridePriority *string) (*ticketSLAResolution, error) {
	const q = `
		SELECT COALESCE($3, cpd.default_priority) AS priority,
		       pol.response_minutes,
		       pol.resolution_minutes,
		       s.franchise_id,
		       (SELECT r.target_role
		          FROM ticket_routing_rules r
		         WHERE (r.category = $2 OR r.category IS NULL)
		           AND (r.franchise_id = s.franchise_id OR r.franchise_id IS NULL)
		         -- id breaks ties so two rules sharing a priority_order resolve
		         -- deterministically rather than by whatever order the planner
		         -- happens to return.
		         ORDER BY r.priority_order ASC, r.id ASC
		         LIMIT 1) AS routed_role
		FROM subscribers s
		LEFT JOIN category_priority_defaults cpd
		       ON cpd.category = $2
		LEFT JOIN sla_policies pol
		       ON pol.category = $2
		      AND pol.priority = COALESCE($3, cpd.default_priority)
		WHERE s.id = $1`

	var (
		res                                ticketSLAResolution
		priority                           *string
		responseMinutes, resolutionMinutes *int
	)
	err := pool.QueryRow(ctx, q, subscriberID, category, overridePriority).
		Scan(&priority, &responseMinutes, &resolutionMinutes, &res.FranchiseID, &res.RoutedRole)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: resolve ticket SLA: subscriber %d does not exist: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: resolve ticket SLA for subscriber %d: %w", subscriberID, err)
	}

	// A missing lookup row is a configuration gap, and ticket creation fails
	// on it rather than quietly producing a ticket with no SLA at all
	// (MDS §4.13 — the same stance FR-NAS takes on a missing vendor profile).
	// Silently defaulting here would mean the breach scanner never sees the
	// ticket and nobody ever learns it was untracked.
	if priority == nil {
		return nil, fmt.Errorf("db: no category_priority_defaults row for category %q (migration 023 seeds one per category)", category)
	}
	if responseMinutes == nil || resolutionMinutes == nil {
		return nil, fmt.Errorf("db: no sla_policies row for category %q priority %q (migration 023 seeds all 16 combinations)", category, *priority)
	}

	res.Priority = *priority
	res.ResponseMinutes = *responseMinutes
	res.ResolutionMinutes = *resolutionMinutes
	return &res, nil
}

// insertTicketSQL is shared by both creation paths.
//
// Both due-at timestamps are computed by PostgreSQL from NOW() in the same
// statement that sets created_at, so all three sit on one clock. Computing
// them in Go would introduce a small skew between created_at and the
// deadlines derived from it, and would put the SLA window on the
// application host's clock rather than the database's.
const insertTicketSQL = `
	INSERT INTO tickets (
		subscriber_id, category, description, status,
		priority, sla_response_due_at, sla_resolution_due_at,
		franchise_id, routed_role
	)
	VALUES (
		$1, $2, $3, 'open',
		$4, NOW() + ($5 * INTERVAL '1 minute'), NOW() + ($6 * INTERVAL '1 minute'),
		$7, $8
	)
	RETURNING ` + ticketColumns
