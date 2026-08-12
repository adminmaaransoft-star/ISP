package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/tickets"
)

// SLAStore serves the SLA breach scanner (FR-SUP-002 | MDS §4.13).
//
// Satisfies tickets.SLAQuerier.
type SLAStore struct{ pool dbPool }

var _ tickets.SLAQuerier = (*SLAStore)(nil)

// SLA returns the store satisfying tickets.SLAQuerier.
func (d *DB) SLA() *SLAStore { return &SLAStore{pool: d.pool} }

// claimSLAEventsSQL finds tickets that have crossed an SLA threshold and
// records the event, in one statement.
//
// The INSERT ... ON CONFLICT DO NOTHING ... RETURNING is the whole
// idempotency mechanism: uq_sla_event (ticket_id, event_type) means a
// second scan over the same breached ticket inserts nothing and returns
// nothing, so an alert fires exactly once per ticket per event type no
// matter how many times the scanner runs. Detecting and recording in
// separate statements would reopen the gap this closes — two scans
// overlapping could both read "breached, not yet recorded" before either
// wrote.
//
// $1 is the warning fraction (0.8 — FR-FUP-004's 80% convention, reused
// rather than a second threshold invented for the same shape of problem).
const claimSLAEventsSQL = `
	WITH candidates AS (
		SELECT t.id,
		       t.subscriber_id,
		       t.priority,
		       t.routed_role,
		       ev.event_type
		  FROM tickets t
		  CROSS JOIN LATERAL (
		      VALUES
		          -- Response clock: only meaningful while nobody has picked
		          -- the ticket up, which is exactly status = 'open'.
		          ('response_breach',    t.sla_response_due_at,   t.status = 'open'),
		          ('response_warning',
		           t.created_at + ($1 * (t.sla_response_due_at - t.created_at)),
		           t.status = 'open'),
		          -- Resolution clock: runs until the ticket actually closes,
		          -- so in_progress still counts as unresolved.
		          ('resolution_breach',  t.sla_resolution_due_at, t.status NOT IN ('resolved','closed')),
		          ('resolution_warning',
		           t.created_at + ($1 * (t.sla_resolution_due_at - t.created_at)),
		           t.status NOT IN ('resolved','closed'))
		  ) AS ev(event_type, threshold_at, clock_applies)
		 WHERE ev.clock_applies
		   AND ev.threshold_at IS NOT NULL   -- pre-migration-023 tickets have no deadlines
		   AND ev.threshold_at <= NOW()
	)
	INSERT INTO sla_events (ticket_id, event_type)
	SELECT c.id, c.event_type FROM candidates c
	ON CONFLICT ON CONSTRAINT uq_sla_event DO NOTHING
	RETURNING ticket_id, event_type,
	          (SELECT subscriber_id FROM tickets WHERE id = ticket_id),
	          (SELECT priority      FROM tickets WHERE id = ticket_id),
	          (SELECT routed_role   FROM tickets WHERE id = ticket_id)`

// ClaimSLAEvents records every newly-crossed SLA threshold and returns only
// the events this call actually recorded — never one a previous scan
// already handled.
func (s *SLAStore) ClaimSLAEvents(ctx context.Context, warningFraction float64) ([]tickets.SLAEvent, error) {
	rows, err := s.pool.Query(ctx, claimSLAEventsSQL, warningFraction)
	if err != nil {
		return nil, fmt.Errorf("db: claim SLA events: %w", err)
	}
	defer rows.Close()

	var out []tickets.SLAEvent
	for rows.Next() {
		var e tickets.SLAEvent
		if err := rows.Scan(&e.TicketID, &e.EventType, &e.SubscriberID, &e.Priority, &e.RoutedRole); err != nil {
			return nil, fmt.Errorf("db: scan SLA event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate SLA events: %w", err)
	}
	return out, nil
}

// CountOpenSLABreaches reports how many tickets are currently in breach of
// each clock, for the console's Support dashboard (FR-SUP-002). Counts live
// tickets rather than sla_events rows: an event is a permanent historical
// record, while this answers "how many are in trouble right now", and a
// ticket resolved after breaching should leave this number.
func (s *SLAStore) CountOpenSLABreaches(ctx context.Context) (response, resolution int, err error) {
	const q = `
		SELECT count(*) FILTER (WHERE status = 'open'
		                          AND sla_response_due_at IS NOT NULL
		                          AND sla_response_due_at <= NOW()),
		       count(*) FILTER (WHERE status NOT IN ('resolved','closed')
		                          AND sla_resolution_due_at IS NOT NULL
		                          AND sla_resolution_due_at <= NOW())
		  FROM tickets`

	if err := s.pool.QueryRow(ctx, q).Scan(&response, &resolution); err != nil {
		return 0, 0, fmt.Errorf("db: count open SLA breaches: %w", err)
	}
	return response, resolution, nil
}
