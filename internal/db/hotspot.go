package db

import (
	"context"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/hotspot"
	"github.com/maaransoft/isp-bss-oss/internal/radius"
)

// Hotspot persistence — FR-HSP-001..003 | migration 034 | MDS §4.23.

// HotspotStore reads and writes hotspot_devices, hotspot_vouchers and
// hotspot_grants.
type HotspotStore struct{ pool dbPool }

var (
	_ radius.MABQuerier  = (*HotspotStore)(nil)
	_ hotspot.GrantStore = (*HotspotStore)(nil)
)

// AuthorizeMAC resolves a MAC to the subscriber it may authenticate as.
//
// Returns (nil, nil) for every refusal — unknown MAC, deactivated device,
// registered against a different NAS, no live grant — because the caller
// answers Access-Reject for all of them and a RADIUS reject carries no reason
// anyway.
//
// Two routes to service, and both are deliberately narrow:
//
//   - A device pre-registered in hotspot_devices to a subscriber. nas_id is
//     matched when set, so a phone enrolled on the café hotspot cannot
//     authenticate on a different operator's NAS that also has MAB on. A NULL
//     nas_id means "any NAS this operator runs", which is the sensible
//     default for a subscriber's own equipment roaming between their sites.
//   - A live captive-portal grant for the MAC (FR-HSP-001), which is how a
//     walk-up user who just completed the walled-garden login gets onto the
//     network without anything pre-registered.
//
// A voucher-backed grant has no subscriber_id, so it resolves through the
// voucher's plan instead — the rate limit still comes from a plan row, which
// is what keeps hotspot sessions shaped by the same machinery as PPPoE.
func (s *HotspotStore) AuthorizeMAC(ctx context.Context, mac string, nasID int) (*radius.Subscriber, error) {
	const q = `
		WITH device_match AS (
			SELECT s.id, s.username, s.password_hash, s.status, s.plan_id,
			       p.rate_limit_string, p.fup_throttle_string, s.fup_active
			  FROM hotspot_devices h
			  JOIN subscribers s ON s.id = h.subscriber_id
			  JOIN plans p       ON p.id = s.plan_id
			 WHERE h.mac_address = $1
			   AND h.active
			   -- NULL nas_id means "any of ours"; a set one must match.
			   AND (h.nas_id IS NULL OR h.nas_id = $2)
		),
		grant_match AS (
			SELECT COALESCE(s.id, 0)                       AS id,
			       COALESCE(s.username, 'voucher:' || g.id::text) AS username,
			       COALESCE(s.password_hash, '')           AS password_hash,
			       COALESCE(s.status, 'active')            AS status,
			       g.plan_id,
			       p.rate_limit_string, p.fup_throttle_string,
			       COALESCE(s.fup_active, FALSE)           AS fup_active
			  FROM hotspot_grants g
			  JOIN plans p          ON p.id = g.plan_id
			  LEFT JOIN subscribers s ON s.id = g.subscriber_id
			 WHERE g.mac_address = $1
			   AND g.revoked_at IS NULL
			   AND g.expires_at > NOW()
			   AND (g.nas_id IS NULL OR g.nas_id = $2)
			 ORDER BY g.expires_at DESC
			 LIMIT 1
		)
		SELECT * FROM device_match
		UNION ALL
		SELECT * FROM grant_match
		LIMIT 1`

	var sub radius.Subscriber
	var fupThrottle *string
	err := s.pool.QueryRow(ctx, q, mac, nasID).Scan(
		&sub.ID, &sub.Username, &sub.PasswordHash, &sub.Status, &sub.PlanID,
		&sub.RateLimitStr, &fupThrottle, &sub.FUPActive)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: authorize mac %s: %w", mac, err)
	}
	if fupThrottle != nil {
		sub.FUPThrottle = *fupThrottle
	}

	// Best-effort: last_seen_at is telemetry for an operator wondering whether
	// a registered device is still in use. Never worth failing an
	// authentication over.
	_, _ = s.pool.Exec(ctx, //nolint:errcheck
		`UPDATE hotspot_devices
		    SET last_seen_at = NOW(),
		        first_seen_at = COALESCE(first_seen_at, NOW())
		  WHERE mac_address = $1`, mac)

	return &sub, nil
}

// RegisterDevice binds a MAC to a subscriber for MAB.
func (s *HotspotStore) RegisterDevice(ctx context.Context, mac string, subscriberID int,
	label string, nasID *int,
) (int, error) {
	const q = `
		INSERT INTO hotspot_devices (mac_address, subscriber_id, label, nas_id)
		VALUES ($1, $2, NULLIF($3,''), $4)
		ON CONFLICT (mac_address) DO UPDATE
		   SET subscriber_id = EXCLUDED.subscriber_id,
		       label         = EXCLUDED.label,
		       nas_id        = EXCLUDED.nas_id,
		       active        = TRUE
		RETURNING id`

	var id int
	if err := s.pool.QueryRow(ctx, q, mac, subscriberID, label, nasID).Scan(&id); err != nil {
		return 0, fmt.Errorf("db: register hotspot device %s: %w", mac, err)
	}
	return id, nil
}

// DeactivateDevice stops a MAC authenticating, without deleting the record.
func (s *HotspotStore) DeactivateDevice(ctx context.Context, mac string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE hotspot_devices SET active = FALSE WHERE mac_address = $1 AND active`, mac)
	if err != nil {
		return false, fmt.Errorf("db: deactivate hotspot device %s: %w", mac, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ── Vouchers ────────────────────────────────────────────────────────────────

// CreateVoucher stores one voucher. The plaintext code never reaches this
// layer — it is shown once at generation, like an API key.
func (s *HotspotStore) CreateVoucher(ctx context.Context, v hotspot.NewVoucher) (int, error) {
	const q = `
		INSERT INTO hotspot_vouchers (
			code_hash, code_prefix, plan_id, franchise_id,
			duration_minutes, data_cap_bytes, expires_at, batch_ref, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8,''), $9)
		RETURNING id`

	var id int
	err := s.pool.QueryRow(ctx, q, v.CodeHash, v.CodePrefix, v.PlanID, v.FranchiseID,
		v.DurationMinutes, v.DataCapBytes, v.ExpiresAt, v.BatchRef, v.CreatedBy).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("db: create voucher: %w", err)
	}
	return id, nil
}

// ListVouchers returns stored vouchers, newest first, optionally narrowed to
// one printed batch or one status.
//
// code_hash is never selected. An operator listing vouchers to reconcile a
// printed batch has no use for it, and a listing endpoint that returned it
// would hand anyone who reaches the admin API the ability to redeem every
// unused code — the exact outcome hashing them was meant to prevent.
func (s *HotspotStore) ListVouchers(ctx context.Context, f hotspot.VoucherFilter) ([]hotspot.Voucher, error) {
	const q = `
		SELECT id, code_prefix, plan_id, franchise_id, duration_minutes, data_cap_bytes,
		       status, COALESCE(redeemed_by_mac, ''), redeemed_at, expires_at, valid_until,
		       COALESCE(batch_ref, ''), created_by, created_at
		  FROM hotspot_vouchers
		 WHERE ($1 = '' OR batch_ref = $1)
		   AND ($2 = '' OR status = $2)
		 ORDER BY created_at DESC, id DESC
		 LIMIT $3`

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, q, f.BatchRef, f.Status, limit)
	if err != nil {
		return nil, fmt.Errorf("db: list vouchers: %w", err)
	}
	defer rows.Close()

	out := make([]hotspot.Voucher, 0, limit)
	for rows.Next() {
		var v hotspot.Voucher
		if err := rows.Scan(&v.ID, &v.CodePrefix, &v.PlanID, &v.FranchiseID,
			&v.DurationMinutes, &v.DataCapBytes, &v.Status, &v.RedeemedByMAC,
			&v.RedeemedAt, &v.ExpiresAt, &v.ValidUntil, &v.BatchRef,
			&v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("db: scan voucher row: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate vouchers: %w", err)
	}
	return out, nil
}

// VoidVoucher takes an unredeemed voucher out of circulation — a printed sheet
// that was lost or mis-issued.
//
// Only 'unused' vouchers can be voided: voiding one that is already redeemed
// would strand a live grant behind a voucher whose status claims it was never
// used, and the schema's chk_voucher_redemption_complete constraint would
// reject the row anyway.
func (s *HotspotStore) VoidVoucher(ctx context.Context, voucherID int) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE hotspot_vouchers SET status = 'void' WHERE id = $1 AND status = 'unused'`, voucherID)
	if err != nil {
		return false, fmt.Errorf("db: void voucher %d: %w", voucherID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RedeemVoucher claims a voucher for a MAC and opens a grant, atomically.
//
// The `status = 'unused'` predicate is the same conditional claim used for
// approvals, CPE tasks and lead conversion. It is what makes a voucher
// single-use under concurrency: two people typing the same printed code at the
// same moment must not both get online, and the loser must be told the code is
// spent rather than silently sharing the winner's session.
//
// Returns (0, nil) when the claim does not land.
func (s *HotspotStore) RedeemVoucher(ctx context.Context, codeHash, mac string, nasID *int) (int64, error) {
	const q = `
		WITH claimed AS (
			UPDATE hotspot_vouchers
			   SET status          = 'used',
			       redeemed_by_mac = $2,
			       redeemed_at     = NOW(),
			       valid_until     = NOW() + (duration_minutes * INTERVAL '1 minute')
			 WHERE code_hash = $1
			   AND status = 'unused'
			   AND (expires_at IS NULL OR expires_at > NOW())
			RETURNING id, plan_id, valid_until
		)
		INSERT INTO hotspot_grants (mac_address, voucher_id, nas_id, plan_id, expires_at)
		SELECT $2, c.id, $3, c.plan_id, c.valid_until FROM claimed c
		RETURNING id`

	var grantID int64
	err := s.pool.QueryRow(ctx, q, codeHash, mac, nasID).Scan(&grantID)
	if isNoRows(err) {
		// Already redeemed, expired, void, or no such code — all reported the
		// same way, since a captive portal must not become an oracle for
		// probing which codes exist.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("db: redeem voucher: %w", err)
	}
	return grantID, nil
}

// GrantForSubscriber opens a captive-portal grant for an authenticated
// subscriber (FR-HSP-001), used when someone logs in at the walled garden
// with their portal credentials rather than a voucher.
func (s *HotspotStore) GrantForSubscriber(ctx context.Context, mac string,
	subscriberID int, nasID *int, minutes int,
) (int64, error) {
	const q = `
		INSERT INTO hotspot_grants (mac_address, subscriber_id, nas_id, plan_id, expires_at)
		SELECT $1, s.id, $2, s.plan_id, NOW() + ($3 * INTERVAL '1 minute')
		  FROM subscribers s
		 WHERE s.id = $4
		   -- The same status gate every other auth path applies. Without it a
		   -- suspended subscriber could get online through the captive portal.
		   AND s.status NOT IN ('hard_suspended', 'terminated')
		RETURNING id`

	var grantID int64
	err := s.pool.QueryRow(ctx, q, mac, nasID, minutes, subscriberID).Scan(&grantID)
	if isNoRows(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("db: grant hotspot access: %w", err)
	}
	return grantID, nil
}

// RevokeGrant ends a grant early.
func (s *HotspotStore) RevokeGrant(ctx context.Context, grantID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE hotspot_grants SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL`, grantID)
	if err != nil {
		return false, fmt.Errorf("db: revoke grant %d: %w", grantID, err)
	}
	return tag.RowsAffected() == 1, nil
}
