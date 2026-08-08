package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/maaransoft/isp-bss-oss/internal/revenue"
	"github.com/shopspring/decimal"
)

// RevenueStore serves the nightly reconciliation job and franchise operations.
// Satisfies revenue.RevenueQuerier, revenue.FranchiseQuerier and
// revenue.SubscriberLister.
type RevenueStore struct{ pool *pgxpool.Pool }

var (
	_ revenue.RevenueQuerier   = (*RevenueStore)(nil)
	_ revenue.FranchiseQuerier = (*RevenueStore)(nil)
	_ revenue.SubscriberLister = (*RevenueStore)(nil)
)

// GetUnbilledActiveSubscribers counts active subscribers whose current billing
// period has no invoice.
//
// FR: FR-REV-001 | uses idx_revenue_unbilled
func (s *RevenueStore) GetUnbilledActiveSubscribers(ctx context.Context) (int, error) {
	const q = `
		SELECT COUNT(*)
		FROM subscribers s
		WHERE s.status = 'active'
		  AND NOT EXISTS (
		      SELECT 1 FROM invoices i
		      WHERE i.subscriber_id = s.id
		        AND i.created_at >= date_trunc('month', NOW())
		  )`

	var count int
	if err := s.pool.QueryRow(ctx, q).Scan(&count); err != nil {
		return 0, fmt.Errorf("db: count unbilled subscribers: %w", err)
	}
	return count, nil
}

// GetLedgerVariance returns the total drift between the wallet ledger and the
// balances denormalised onto subscribers.
//
// Only the subscriber_wallet leg is summed. Including the gateway clearing leg
// would net every recharge to zero and make the check structurally unable to
// detect a discrepancy.
//
// FR: FR-REV-002
func (s *RevenueStore) GetLedgerVariance(ctx context.Context) (decimal.Decimal, error) {
	const q = `
		WITH ledger AS (
			SELECT subscriber_id,
			       SUM(CASE WHEN entry_type = 'credit' THEN amount ELSE -amount END) AS net
			FROM wallet_ledgers
			WHERE account = 'subscriber_wallet'
			GROUP BY subscriber_id
		)
		SELECT COALESCE(SUM(s.wallet_balance - COALESCE(l.net, 0)), 0)::text
		FROM subscribers s
		LEFT JOIN ledger l ON l.subscriber_id = s.id`

	var variance string
	if err := s.pool.QueryRow(ctx, q).Scan(&variance); err != nil {
		return decimal.Zero, fmt.Errorf("db: compute ledger variance: %w", err)
	}
	return parseDecimal(variance)
}

// GetTotalWalletBalance sums every subscriber's wallet balance.
func (s *RevenueStore) GetTotalWalletBalance(ctx context.Context) (decimal.Decimal, error) {
	const q = `SELECT COALESCE(SUM(wallet_balance), 0)::text FROM subscribers`

	var total string
	if err := s.pool.QueryRow(ctx, q).Scan(&total); err != nil {
		return decimal.Zero, fmt.Errorf("db: sum wallet balances: %w", err)
	}
	return parseDecimal(total)
}

// UpsertRevenueSnapshot writes the day's snapshot, replacing any earlier run for
// the same date so a re-run does not double-count.
func (s *RevenueStore) UpsertRevenueSnapshot(ctx context.Context, snap revenue.RevenueSnapshot) error {
	const q = `
		INSERT INTO revenue_snapshots (
			snapshot_date, unbilled_subscriber_count, ledger_variance, total_wallet_balance
		) VALUES ($1::date, $2, $3::numeric, $4::numeric)`

	const clear = `DELETE FROM revenue_snapshots WHERE snapshot_date = $1::date`

	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, clear, snap.SnapshotDate); err != nil {
			return fmt.Errorf("db: clear revenue snapshot: %w", err)
		}
		if _, err := tx.Exec(ctx, q,
			snap.SnapshotDate, snap.UnbilledSubscriberCount,
			snap.LedgerVariance.String(), snap.TotalWalletBalance.String(),
		); err != nil {
			return fmt.Errorf("db: insert revenue snapshot: %w", err)
		}
		return nil
	})
}

// BuildCollectionsForecast projects renewals for the next `days` days, splitting
// each day into subscribers expected to renew and those at risk.
//
// At risk means the wallet cannot cover the plan price, or the account is
// already in a suspension stage — the two cases where a renewal silently fails.
//
// FR: FR-REV-004
func (s *RevenueStore) BuildCollectionsForecast(ctx context.Context, days int) ([]revenue.CollectionsForecast, error) {
	if days <= 0 {
		days = 30
	}
	const q = `
		SELECT CURRENT_DATE                              AS forecast_date,
		       s.plan_expiry::date                       AS forecast_for_date,
		       COUNT(*) FILTER (WHERE NOT at_risk)       AS expected_renewals,
		       COUNT(*) FILTER (WHERE at_risk)           AS at_risk_renewals,
		       COALESCE(SUM(p.price) FILTER (WHERE NOT at_risk), 0)::text AS expected_revenue,
		       COALESCE(SUM(p.price) FILTER (WHERE at_risk), 0)::text     AS at_risk_revenue
		FROM (
			SELECT s.id, s.plan_id, s.plan_expiry,
			       (s.wallet_balance < p.price
			        OR s.dunning_state IN ('soft_suspended','hard_suspended','grace_period')) AS at_risk
			FROM subscribers s
			JOIN plans p ON p.id = s.plan_id
			WHERE s.plan_expiry IS NOT NULL
			  AND s.plan_expiry::date BETWEEN CURRENT_DATE AND CURRENT_DATE + ($1::int)
			  AND s.status <> 'terminated'
		) s
		JOIN plans p ON p.id = s.plan_id
		GROUP BY s.plan_expiry::date
		ORDER BY forecast_for_date`

	rows, err := s.pool.Query(ctx, q, days)
	if err != nil {
		return nil, fmt.Errorf("db: build collections forecast: %w", err)
	}
	defer rows.Close()

	forecasts := make([]revenue.CollectionsForecast, 0, days)
	for rows.Next() {
		var (
			f                      revenue.CollectionsForecast
			expectedRev, atRiskRev string
		)
		if err := rows.Scan(&f.ForecastDate, &f.ForecastForDate,
			&f.ExpectedRenewals, &f.AtRiskRenewals, &expectedRev, &atRiskRev); err != nil {
			return nil, fmt.Errorf("db: scan forecast row: %w", err)
		}
		if f.ExpectedRevenue, err = parseDecimal(expectedRev); err != nil {
			return nil, err
		}
		if f.AtRiskRevenue, err = parseDecimal(atRiskRev); err != nil {
			return nil, err
		}
		forecasts = append(forecasts, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate forecast rows: %w", err)
	}
	return forecasts, nil
}

// UpsertCollectionsForecast replaces the forecast generated today.
func (s *RevenueStore) UpsertCollectionsForecast(ctx context.Context, forecasts []revenue.CollectionsForecast) error {
	const clear = `DELETE FROM collections_forecast WHERE forecast_date = CURRENT_DATE`
	const insert = `
		INSERT INTO collections_forecast (
			forecast_date, forecast_for_date, expected_renewals, at_risk_renewals,
			expected_revenue, at_risk_revenue
		) VALUES ($1::date, $2::date, $3, $4, $5::numeric, $6::numeric)`

	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, clear); err != nil {
			return fmt.Errorf("db: clear collections forecast: %w", err)
		}
		for _, f := range forecasts {
			if _, err := tx.Exec(ctx, insert,
				f.ForecastDate, f.ForecastForDate, f.ExpectedRenewals, f.AtRiskRenewals,
				f.ExpectedRevenue.String(), f.AtRiskRevenue.String(),
			); err != nil {
				return fmt.Errorf("db: insert collections forecast: %w", err)
			}
		}
		return nil
	})
}

// ── Franchise ───────────────────────────────────────────────────────────────

// GetFranchiseByID loads one franchise.
func (s *RevenueStore) GetFranchiseByID(ctx context.Context, franchiseID int) (*revenue.Franchise, error) {
	const q = `SELECT id, name, commission_rate_pct::text, status FROM franchises WHERE id = $1`

	var (
		f    revenue.Franchise
		rate string
	)
	err := s.pool.QueryRow(ctx, q, franchiseID).Scan(&f.ID, &f.Name, &rate, &f.Status)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: franchise %d: %w", franchiseID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get franchise %d: %w", franchiseID, err)
	}
	if f.CommissionRatePct, err = parseDecimal(rate); err != nil {
		return nil, err
	}
	return &f, nil
}

// CalculateAndStoreLCOCommission writes one lco_ledger row.
//
// The commission is computed by the caller (revenue.CalculateLCOCommission) and
// passed in already rounded, so the stored figure is exactly what was reported.
func (s *RevenueStore) CalculateAndStoreLCOCommission(ctx context.Context, entry revenue.LCOCommissionEntry) error {
	const q = `
		INSERT INTO lco_ledger (
			franchise_id, subscriber_id, recharge_amount, commission_amount, transaction_ref
		) VALUES ($1, $2, $3::numeric, $4::numeric, NULLIF($5,''))`

	if _, err := s.pool.Exec(ctx, q,
		entry.FranchiseID, entry.SubscriberID,
		entry.RechargeAmount.String(), entry.CommissionAmount.String(), entry.TransactionRef,
	); err != nil {
		return fmt.Errorf("db: store LCO commission for franchise %d: %w", entry.FranchiseID, err)
	}
	return nil
}

// ListSubscribers returns subscribers within a franchise scope. A nil
// franchiseID means unrestricted (ISP-wide) visibility.
//
// FR: FR-FRN-001 | uses idx_franchise_subscribers
func (s *RevenueStore) ListSubscribers(ctx context.Context, franchiseID *int) ([]revenue.SubscriberRow, error) {
	// One statement with a NULL-guarded predicate rather than two: a second
	// query string is a second place for the scope filter to be forgotten.
	const q = `
		SELECT id, username, franchise_id
		FROM subscribers
		WHERE ($1::int IS NULL OR franchise_id = $1::int)
		ORDER BY id`

	rows, err := s.pool.Query(ctx, q, franchiseID)
	if err != nil {
		return nil, fmt.Errorf("db: list subscribers: %w", err)
	}
	defer rows.Close()

	out := make([]revenue.SubscriberRow, 0, 32)
	for rows.Next() {
		var r revenue.SubscriberRow
		if err := rows.Scan(&r.ID, &r.Username, &r.FranchiseID); err != nil {
			return nil, fmt.Errorf("db: scan subscriber row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate subscribers: %w", err)
	}
	return out, nil
}
