package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/api"
)

// TicketStore serves the admin ticket endpoints. Distinct from PortalStore's
// ticket methods, which are scoped to the calling subscriber: admin actions
// create on behalf of an arbitrary subscriber and can set assigned_to, neither
// of which the self-service surface should ever be able to do.
//
// Satisfies api.TicketAdminQuerier.
type TicketStore struct{ pool dbPool }

var _ api.TicketAdminQuerier = (*TicketStore)(nil)

const ticketColumns = `id, subscriber_id, category, description, status, assigned_to, created_at, updated_at,
	priority, sla_response_due_at, sla_resolution_due_at, routed_role`

func scanTicket(row interface{ Scan(dest ...any) error }) (*api.TicketRecord, error) {
	var (
		t          api.TicketRecord
		assignedTo *int
	)
	if err := row.Scan(&t.ID, &t.SubscriberID, &t.Category, &t.Description, &t.Status,
		&assignedTo, &t.CreatedAt, &t.UpdatedAt,
		&t.Priority, &t.SLAResponseDueAt, &t.SLAResolutionDueAt, &t.RoutedRole); err != nil {
		return nil, err
	}
	t.AssignedTo = assignedTo
	return &t, nil
}

// CreateTicketAdmin files a ticket on behalf of subscriberID.
//
// priority is optional: nil takes the category's default. Only staff reach
// this method (the portal has its own, which never accepts an override) so
// an explicit priority here is a triage decision, not a reporter's
// self-assessment (MDS §4.13).
func (s *TicketStore) CreateTicketAdmin(ctx context.Context, subscriberID int, category, description string, priority *string) (*api.TicketRecord, error) {
	sla, err := resolveTicketSLA(ctx, s.pool, subscriberID, category, priority)
	if err != nil {
		return nil, err
	}

	t, err := scanTicket(s.pool.QueryRow(ctx, insertTicketSQL,
		subscriberID, category, description,
		sla.Priority, sla.ResponseMinutes, sla.ResolutionMinutes,
		sla.FranchiseID, sla.RoutedRole))
	if err != nil {
		return nil, fmt.Errorf("db: create ticket for subscriber %d: %w", subscriberID, err)
	}
	return t, nil
}

// UpdateTicketAdmin applies a partial update. A nil field is left untouched.
// Returns (nil, nil) when ticketID does not exist.
//
// A priority change re-derives both SLA deadlines from the ticket's original
// created_at, never from now() (FR-SUP-001 | MDS §4.13). Re-anchoring to the
// current time would let repeated re-triage push a deadline out indefinitely,
// which is the one way an SLA clock can be made meaningless without anyone
// noticing.
func (s *TicketStore) UpdateTicketAdmin(ctx context.Context, ticketID int, status *string, assignedTo *int, priority *string) (*api.TicketRecord, error) {
	// The sla_policies lookup happens inside the CTE rather than in the
	// UPDATE's FROM clause: PostgreSQL does not allow the UPDATE target
	// table to be referenced from a join condition in its own FROM clause
	// ("invalid reference to FROM-clause entry", SQLSTATE 42P01), so
	// pol.category has to be matched against the CTE's copy of the category,
	// not against tickets directly.
	const q = `
		WITH resolved AS (
			SELECT t.id,
			       t.created_at,
			       t.category,
			       COALESCE($4, t.priority) AS new_priority
			  FROM tickets t
			 WHERE t.id = $1
		),
		policy AS (
			-- Columns are renamed rather than passed through as id/created_at:
			-- RETURNING below sees both this CTE and tickets, so a column
			-- sharing a name with one of tickets' own is ambiguous
			-- (SQLSTATE 42702) even though the UPDATE itself is unambiguous.
			SELECT r.id           AS ticket_id,
			       r.created_at   AS anchor_created_at,
			       r.new_priority,
			       pol.response_minutes,
			       pol.resolution_minutes
			  FROM resolved r
			  -- LEFT JOIN, not inner: with no priority override there is
			  -- nothing to look up, and an inner join would make every
			  -- ordinary status change depend on an sla_policies row.
			  LEFT JOIN sla_policies pol
			         ON pol.category = r.category
			        AND pol.priority = r.new_priority
		)
		UPDATE tickets t
		SET status      = COALESCE($2, t.status),
		    assigned_to = COALESCE($3, t.assigned_to),
		    priority    = p.new_priority,
		    sla_response_due_at = CASE
		        WHEN $4::text IS NULL THEN t.sla_response_due_at
		        ELSE p.anchor_created_at + (p.response_minutes * INTERVAL '1 minute')
		    END,
		    sla_resolution_due_at = CASE
		        WHEN $4::text IS NULL THEN t.sla_resolution_due_at
		        ELSE p.anchor_created_at + (p.resolution_minutes * INTERVAL '1 minute')
		    END
		FROM policy p
		WHERE t.id = p.ticket_id
		RETURNING ` + ticketColumns

	t, err := scanTicket(s.pool.QueryRow(ctx, q, ticketID, status, assignedTo, priority))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update ticket %d: %w", ticketID, err)
	}
	// A priority override with no matching sla_policies row leaves both
	// deadlines NULL rather than failing — which would silently drop the
	// ticket out of the breach scanner's view. Caught here instead.
	if priority != nil && (t.SLAResponseDueAt == nil || t.SLAResolutionDueAt == nil) {
		return nil, fmt.Errorf("db: update ticket %d: no sla_policies row for category %q priority %q", ticketID, t.Category, *priority)
	}
	return t, nil
}
