package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/api"
	"github.com/maaransoft/isp-bss-oss/internal/health"
	"github.com/maaransoft/isp-bss-oss/internal/portal"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
	"github.com/shopspring/decimal"
)

// ── RADIUS ──────────────────────────────────────────────────────────────────

// RadiusStore serves the AAA hot path. Satisfies radius.DBQuerier.
type RadiusStore struct{ pool *pgxpool.Pool }

var _ radius.DBQuerier = (*RadiusStore)(nil)

// GetSubscriberByUsername loads the fields an Access-Request decision needs.
//
// The join to plans supplies the rate limit and FUP throttle profile; a
// subscriber whose plan row is missing would be unauthenticatable, so the join
// is inner by design.
//
// This query runs on every authentication and is covered by idx_sub_auth
// (username, status) to hold NFR-PERF-001's 15ms p99.
func (s *RadiusStore) GetSubscriberByUsername(ctx context.Context, username string) (*radius.Subscriber, error) {
	const q = `
		SELECT s.id, s.username, s.password_hash, s.status,
		       p.rate_limit_string, COALESCE(p.fup_throttle_string, ''),
		       COALESCE(s.fup_active, FALSE)
		FROM subscribers s
		JOIN plans p ON p.id = s.plan_id
		WHERE s.username = $1`

	var sub radius.Subscriber
	err := s.pool.QueryRow(ctx, q, username).Scan(
		&sub.ID, &sub.Username, &sub.PasswordHash, &sub.Status,
		&sub.RateLimitStr, &sub.FUPThrottle, &sub.FUPActive,
	)
	if isNoRows(err) {
		// A missing subscriber is a normal reject, not a failure: handleAuth
		// treats (nil, nil) as "no such user".
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get subscriber by username %q: %w", username, err)
	}
	return &sub, nil
}

// ── API ─────────────────────────────────────────────────────────────────────

// APIStore serves the admin API. Satisfies api.SubscriberQuerier and api.KYCQuerier.
type APIStore struct{ pool *pgxpool.Pool }

var (
	_ api.SubscriberQuerier = (*APIStore)(nil)
	_ api.KYCQuerier        = (*APIStore)(nil)
)

const apiSubscriberColumns = `
	s.id, s.caf_number, s.username, s.mobile_number, COALESCE(s.email, ''),
	s.plan_id, s.franchise_id, s.status, s.dunning_state,
	s.wallet_balance::text, s.registered_state, s.kyc_status,
	s.plan_expiry, s.created_at`

func scanAPISubscriber(row interface {
	Scan(dest ...any) error
}) (*api.SubscriberRecord, error) {
	var (
		rec     api.SubscriberRecord
		balance string
	)
	err := row.Scan(
		&rec.ID, &rec.CAFNumber, &rec.Username, &rec.MobileNumber, &rec.Email,
		&rec.PlanID, &rec.FranchiseID, &rec.Status, &rec.DunningState,
		&balance, &rec.RegisteredState, &rec.KYCStatus,
		&rec.PlanExpiry, &rec.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	// The API exposes the balance as a fixed-2dp string so JSON consumers never
	// see it as a float.
	amount, err := parseDecimal(balance)
	if err != nil {
		return nil, err
	}
	rec.WalletBalance = amount.StringFixed(2)
	return &rec, nil
}

// CreateSubscriber inserts a subscriber and returns the persisted row.
func (s *APIStore) CreateSubscriber(ctx context.Context, sub api.SubscriberRecord, passwordHash string) (*api.SubscriberRecord, error) {
	// The shared column list is written against the alias "s", so the insert is
	// wrapped in a CTE that both the write and the projection can reference.
	const q = `
		WITH ins AS (
			INSERT INTO subscribers (
				caf_number, username, password_hash, mobile_number, email,
				plan_id, franchise_id, status, dunning_state, wallet_balance,
				registered_state, kyc_status, plan_expiry
			) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,COALESCE(NULLIF($10,''),'0.00')::numeric,$11,$12,$13)
			RETURNING *
		)
		SELECT ` + apiSubscriberColumns + ` FROM ins s`

	status := sub.Status
	if status == "" {
		status = "active"
	}
	dunning := sub.DunningState
	if dunning == "" {
		dunning = "active"
	}
	kyc := sub.KYCStatus
	if kyc == "" {
		kyc = "pending"
	}

	row := s.pool.QueryRow(ctx, q,
		sub.CAFNumber, sub.Username, passwordHash, sub.MobileNumber, sub.Email,
		sub.PlanID, sub.FranchiseID, status, dunning, sub.WalletBalance,
		sub.RegisteredState, kyc, sub.PlanExpiry,
	)
	rec, err := scanAPISubscriber(row)
	if err != nil {
		// Surfaced verbatim so api.isUniqueViolation can classify a duplicate
		// caf_number or username as 409 rather than 500.
		return nil, fmt.Errorf("db: create subscriber %q: %w", sub.Username, err)
	}
	return rec, nil
}

// GetSubscriberByID loads one subscriber. A missing row returns (nil, nil),
// which the API renders as 404.
func (s *APIStore) GetSubscriberByID(ctx context.Context, id int) (*api.SubscriberRecord, error) {
	const q = `SELECT ` + apiSubscriberColumns + ` FROM subscribers s WHERE s.id = $1`
	rec, err := scanAPISubscriber(s.pool.QueryRow(ctx, q, id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get subscriber %d: %w", id, err)
	}
	return rec, nil
}

// GetSubscriberByUsername loads one subscriber by login name.
func (s *APIStore) GetSubscriberByUsername(ctx context.Context, username string) (*api.SubscriberRecord, error) {
	const q = `SELECT ` + apiSubscriberColumns + ` FROM subscribers s WHERE s.username = $1`
	rec, err := scanAPISubscriber(s.pool.QueryRow(ctx, q, username))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get subscriber by username %q: %w", username, err)
	}
	return rec, nil
}

// UpdateSubscriber applies a partial update. A nil field is left untouched.
func (s *APIStore) UpdateSubscriber(ctx context.Context, id int, planID *int, status *string) (*api.SubscriberRecord, error) {
	const q = `
		WITH upd AS (
			UPDATE subscribers
			SET plan_id = COALESCE($2, plan_id),
			    status  = COALESCE($3, status)
			WHERE id = $1
			RETURNING *
		)
		SELECT ` + apiSubscriberColumns + ` FROM upd s`

	rec, err := scanAPISubscriber(s.pool.QueryRow(ctx, q, id, planID, status))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: update subscriber %d: %w", id, err)
	}
	return rec, nil
}

// UpsertKYC stores the encrypted Aadhaar and PAN against a subscriber.
//
// Only ciphertext reaches this method: encryption happens in the API handler so
// plaintext PII never enters the persistence layer (FR-SEC-002).
func (s *APIStore) UpsertKYC(ctx context.Context, subscriberID int, aadhaarEnc, panEnc, keyVersion string) error {
	// The column is key_version_id and carries an FK to encryption_keys(version_id),
	// so the key version must be registered before any ciphertext referencing it
	// can be stored. uq_kyc_subscriber (migration 016) provides the conflict target.
	const q = `
		INSERT INTO kyc_verifications (subscriber_id, aadhaar_encrypted, pan_encrypted, key_version_id)
		VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4)
		ON CONFLICT ON CONSTRAINT uq_kyc_subscriber DO UPDATE
		SET aadhaar_encrypted = COALESCE(EXCLUDED.aadhaar_encrypted, kyc_verifications.aadhaar_encrypted),
		    pan_encrypted     = COALESCE(EXCLUDED.pan_encrypted,     kyc_verifications.pan_encrypted),
		    key_version_id    = EXCLUDED.key_version_id`

	if _, err := s.pool.Exec(ctx, q, subscriberID, aadhaarEnc, panEnc, keyVersion); err != nil {
		return fmt.Errorf("db: upsert KYC for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// ── Health ──────────────────────────────────────────────────────────────────

// HealthStore serves the single-call diagnostic endpoint. Satisfies health.DBQuerier.
type HealthStore struct{ pool *pgxpool.Pool }

var _ health.DBQuerier = (*HealthStore)(nil)

// GetSubscriberWithMeta assembles account state plus open-ticket count and the
// most recent notification in one round trip.
//
// It is one query rather than three because the endpoint has a 200ms budget
// (NFR-PERF-002) and already spends part of it on a concurrent Redis lookup.
func (s *HealthStore) GetSubscriberWithMeta(ctx context.Context, subscriberID int) (*health.SubscriberRecord, error) {
	const q = `
		SELECT s.id, s.username, s.status, s.wallet_balance::text, s.plan_expiry,
		       s.dnd_opt_out,
		       (SELECT COUNT(*) FROM tickets t
		         WHERE t.subscriber_id = s.id AND t.status IN ('open','in_progress')),
		       COALESCE((SELECT n.triggered_by_event FROM notification_log n
		                  WHERE n.subscriber_id = s.id
		                  ORDER BY n.sent_at DESC LIMIT 1), ''),
		       (SELECT n.sent_at FROM notification_log n
		         WHERE n.subscriber_id = s.id
		         ORDER BY n.sent_at DESC LIMIT 1)
		FROM subscribers s
		WHERE s.id = $1`

	var (
		rec     health.SubscriberRecord
		balance string
	)
	err := s.pool.QueryRow(ctx, q, subscriberID).Scan(
		&rec.ID, &rec.Username, &rec.Status, &balance, &rec.PlanExpiry,
		&rec.DndOptOut, &rec.OpenTickets, &rec.LastNotifEvent, &rec.LastNotifAt,
	)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: subscriber %d: %w", subscriberID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get subscriber health %d: %w", subscriberID, err)
	}
	if rec.WalletBalance, err = parseDecimal(balance); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ── Portal ──────────────────────────────────────────────────────────────────

// PortalStore serves the subscriber self-service portal. Satisfies
// portal.PortalSubscriberQuerier, portal.PortalNotificationQuerier and
// portal.PortalTicketQuerier.
type PortalStore struct{ pool *pgxpool.Pool }

var (
	_ portal.PortalSubscriberQuerier     = (*PortalStore)(nil)
	_ portal.PortalNotificationQuerier   = (*PortalStore)(nil)
	_ portal.PortalTicketQuerier         = (*PortalStore)(nil)
	_ portal.PortalSessionHistoryQuerier = (*PortalStore)(nil)
	_ portal.PlanExpiryStore             = (*PortalStore)(nil)
)

// GetSubscriberByUsername loads the credentials for portal login.
//
// It returns only id, username and password hash: a login attempt has no reason
// to pull PII or balances into memory.
func (s *PortalStore) GetSubscriberByUsername(ctx context.Context, username string) (*portal.SubscriberAuth, error) {
	const q = `SELECT id, username, password_hash FROM subscribers WHERE username = $1`

	var auth portal.SubscriberAuth
	err := s.pool.QueryRow(ctx, q, username).Scan(&auth.ID, &auth.Username, &auth.PasswordHash)
	if isNoRows(err) {
		// Login treats (nil, nil) as "unknown user" and still runs a dummy
		// bcrypt comparison, so timing does not reveal whether the account exists.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: portal login lookup %q: %w", username, err)
	}
	return &auth, nil
}

// GetSubscriberByID loads the subscriber's own profile view.
func (s *PortalStore) GetSubscriberByID(ctx context.Context, id int) (*portal.SubscriberProfile, error) {
	const q = `
		SELECT s.id, s.username, s.mobile_number, p.name,
		       s.plan_expiry, s.wallet_balance::text, s.status, s.dunning_state
		FROM subscribers s
		JOIN plans p ON p.id = s.plan_id
		WHERE s.id = $1`

	var (
		profile portal.SubscriberProfile
		balance string
	)
	err := s.pool.QueryRow(ctx, q, id).Scan(
		&profile.ID, &profile.Username, &profile.MobileNumber, &profile.PlanName,
		&profile.PlanExpiry, &balance, &profile.Status, &profile.DunningState,
	)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: portal profile %d: %w", id, err)
	}
	if profile.WalletBalance, err = parseDecimal(balance); err != nil {
		return nil, err
	}
	return &profile, nil
}

// ListNotifications returns the subscriber's own notification history, newest
// first. Scoping is by subscriber_id from the caller's JWT (FR-SUB-005).
func (s *PortalStore) ListNotifications(ctx context.Context, subscriberID, limit int) ([]portal.NotificationEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `
		SELECT n.id, n.channel, COALESCE(t.template_name, ''),
		       COALESCE(t.event_trigger, n.triggered_by_event),
		       n.delivery_status, n.sent_at
		FROM notification_log n
		LEFT JOIN notification_templates t ON t.id = n.template_id
		WHERE n.subscriber_id = $1
		ORDER BY n.sent_at DESC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, subscriberID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list notifications for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	entries := make([]portal.NotificationEntry, 0, limit)
	for rows.Next() {
		var e portal.NotificationEntry
		if err := rows.Scan(&e.ID, &e.Channel, &e.TemplateName, &e.Class, &e.DeliveryStatus, &e.SentAt); err != nil {
			return nil, fmt.Errorf("db: scan notification row: %w", err)
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate notifications: %w", err)
	}
	return entries, nil
}

// ListTickets returns the subscriber's own tickets, newest first.
func (s *PortalStore) ListTickets(ctx context.Context, subscriberID int) ([]portal.TicketEntry, error) {
	const q = `
		SELECT id, category, description, status, created_at
		FROM tickets
		WHERE subscriber_id = $1
		ORDER BY created_at DESC`

	rows, err := s.pool.Query(ctx, q, subscriberID)
	if err != nil {
		return nil, fmt.Errorf("db: list tickets for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	tickets := make([]portal.TicketEntry, 0, 8)
	for rows.Next() {
		var t portal.TicketEntry
		if err := rows.Scan(&t.ID, &t.Category, &t.Description, &t.Status, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan ticket row: %w", err)
		}
		tickets = append(tickets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate tickets: %w", err)
	}
	return tickets, nil
}

// CreateTicket files a ticket against req.SubscriberID, which the handler sets
// from the authenticated JWT rather than the request body.
func (s *PortalStore) CreateTicket(ctx context.Context, req portal.TicketCreateRequest) (*portal.TicketEntry, error) {
	const q = `
		INSERT INTO tickets (subscriber_id, category, description, status)
		VALUES ($1, $2, $3, 'open')
		RETURNING id, category, description, status, created_at`

	var t portal.TicketEntry
	err := s.pool.QueryRow(ctx, q, req.SubscriberID, req.Category, req.Description).
		Scan(&t.ID, &t.Category, &t.Description, &t.Status, &t.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("db: create ticket for subscriber %d: %w", req.SubscriberID, err)
	}
	return &t, nil
}

// SetPlanExpiry extends a subscriber's plan validity after a successful renewal.
func (s *PortalStore) SetPlanExpiry(ctx context.Context, subscriberID int, expiry time.Time) error {
	const q = `UPDATE subscribers SET plan_expiry = $2 WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, subscriberID, expiry); err != nil {
		return fmt.Errorf("db: set plan expiry for subscriber %d: %w", subscriberID, err)
	}
	return nil
}

// GetPlanRenewalInfo returns the validity window (in days) of the
// subscriber's current plan, and their current plan_expiry (nil if never
// set) — the two inputs a renewal needs to compute where the plan should
// extend to.
func (s *PortalStore) GetPlanRenewalInfo(ctx context.Context, subscriberID int) (int, *time.Time, error) {
	const q = `
		SELECT p.validity_days, s.plan_expiry
		FROM subscribers s
		JOIN plans p ON p.id = s.plan_id
		WHERE s.id = $1`

	var (
		validityDays  int
		currentExpiry *time.Time
	)
	err := s.pool.QueryRow(ctx, q, subscriberID).Scan(&validityDays, &currentExpiry)
	if err != nil {
		return 0, nil, fmt.Errorf("db: get plan renewal info for subscriber %d: %w", subscriberID, err)
	}
	return validityDays, currentExpiry, nil
}

// bytesPerGB matches the divisor internal/cache.SessionStore.PortalSession
// uses for the live-session usage figures, so history and live numbers are
// never computed on two different definitions of "a GB".
const bytesPerGB = 1024 * 1024 * 1024

// ListSessionHistory returns a subscriber's past internet sessions, newest
// first, including the currently active one if there is one (stop_time NULL).
// Covered by idx_session_history_subscriber (migration 018).
func (s *PortalStore) ListSessionHistory(ctx context.Context, subscriberID, limit int) ([]portal.SessionHistoryEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `
		SELECT session_id, nas_ip_address::text, COALESCE(assigned_ipv4::text, ''),
		       start_time, stop_time, input_octets, output_octets, COALESCE(terminate_cause, '')
		FROM subscriber_session_history
		WHERE subscriber_id = $1
		ORDER BY start_time DESC
		LIMIT $2`

	rows, err := s.pool.Query(ctx, q, subscriberID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list session history for subscriber %d: %w", subscriberID, err)
	}
	defer rows.Close()

	entries := make([]portal.SessionHistoryEntry, 0, limit)
	for rows.Next() {
		var (
			e                         portal.SessionHistoryEntry
			inputOctets, outputOctets int64
		)
		if err := rows.Scan(
			&e.SessionID, &e.NASIP, &e.AssignedIP,
			&e.StartTime, &e.StopTime,
			&inputOctets, &outputOctets, &e.TerminateCause,
		); err != nil {
			return nil, fmt.Errorf("db: scan session history row: %w", err)
		}
		e.GBUsed = decimal.NewFromInt(inputOctets + outputOctets).Div(decimal.NewFromInt(bytesPerGB)).Round(2)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate session history: %w", err)
	}
	return entries, nil
}
