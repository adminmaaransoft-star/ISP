package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/api"
)

// TicketStore serves the admin ticket endpoints. Distinct from PortalStore's
// ticket methods, which are scoped to the calling subscriber: admin actions
// create on behalf of an arbitrary subscriber and can set assigned_to, neither
// of which the self-service surface should ever be able to do.
//
// Satisfies api.TicketAdminQuerier.
type TicketStore struct{ pool *pgxpool.Pool }

var _ api.TicketAdminQuerier = (*TicketStore)(nil)

const ticketColumns = `id, subscriber_id, category, description, status, assigned_to, created_at, updated_at`

func scanTicket(row interface{ Scan(dest ...any) error }) (*api.TicketRecord, error) {
	var (
		t          api.TicketRecord
		assignedTo *int
	)
	if err := row.Scan(&t.ID, &t.SubscriberID, &t.Category, &t.Description, &t.Status,
		&assignedTo, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.AssignedTo = assignedTo
	return &t, nil
}

// CreateTicketAdmin files a ticket on behalf of subscriberID.
func (s *TicketStore) CreateTicketAdmin(ctx context.Context, subscriberID int, category, description string) (*api.TicketRecord, error) {
	const q = `
		INSERT INTO tickets (subscriber_id, category, description, status)
		VALUES ($1, $2, $3, 'open')
		RETURNING ` + ticketColumns

	t, err := scanTicket(s.pool.QueryRow(ctx, q, subscriberID, category, description))
	if err != nil {
		return nil, fmt.Errorf("db: create ticket for subscriber %d: %w", subscriberID, err)
	}
	return t, nil
}

// UpdateTicketAdmin applies a partial update. A nil field is left untouched.
// Returns (nil, nil) when ticketID does not exist.
func (s *TicketStore) UpdateTicketAdmin(ctx context.Context, ticketID int, status *string, assignedTo *int) (*api.TicketRecord, error) {
	const q = `
		UPDATE tickets
		SET status      = COALESCE($2, status),
		    assigned_to = COALESCE($3, assigned_to)
		WHERE id = $1
		RETURNING ` + ticketColumns

	t, err := scanTicket(s.pool.QueryRow(ctx, q, ticketID, status, assignedTo))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update ticket %d: %w", ticketID, err)
	}
	return t, nil
}
