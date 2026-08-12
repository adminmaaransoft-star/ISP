package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/maaransoft/isp-bss-oss/internal/nas"
)

// NASStore serves the multi-vendor NAS attribute engine: the registered
// device inventory (nas.Resolver's cache source) and per-plan vendor
// profile mappings for policy-reference vendors.
//
// Satisfies nas.DeviceStore.
type NASStore struct{ pool dbPool }

var _ nas.DeviceStore = (*NASStore)(nil)

// ListNASDevices returns every registered NAS, secret still encrypted —
// nas.Resolver decrypts on load, keeping plaintext secrets out of this
// package entirely (FR-SEC-002's PII-encryption discipline extended to the
// RADIUS shared secret, which is exactly as sensitive).
func (s *NASStore) ListNASDevices(ctx context.Context) ([]nas.DeviceRow, error) {
	const q = `
		SELECT host(ip), vendor, radius_secret_encrypted, coa_port, pod_port
		FROM nas_devices`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list nas_devices: %w", err)
	}
	defer rows.Close()

	var out []nas.DeviceRow
	for rows.Next() {
		var row nas.DeviceRow
		if err := rows.Scan(&row.IP, &row.Vendor, &row.RadiusSecretEncrypted, &row.CoAPort, &row.PoDPort); err != nil {
			return nil, fmt.Errorf("db: scan nas_devices row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate nas_devices: %w", err)
	}
	return out, nil
}

// ListPlanNASProfiles returns every plan-to-vendor-profile mapping, for
// nas.Resolver's cache (the same small-dataset, refresh-on-interval
// reasoning as ListNASDevices — a handful of plans times a handful of
// vendors, not a per-subscriber-scale table).
func (s *NASStore) ListPlanNASProfiles(ctx context.Context) ([]nas.PlanProfileRow, error) {
	const q = `SELECT plan_id, vendor, profile_name FROM plan_nas_profiles`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("db: list plan_nas_profiles: %w", err)
	}
	defer rows.Close()

	var out []nas.PlanProfileRow
	for rows.Next() {
		var row nas.PlanProfileRow
		if err := rows.Scan(&row.PlanID, &row.Vendor, &row.ProfileName); err != nil {
			return nil, fmt.Errorf("db: scan plan_nas_profiles row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate plan_nas_profiles: %w", err)
	}
	return out, nil
}

// GetPlanNASProfile returns the pre-provisioned QoS profile/role name a
// plan maps to for a policy-reference vendor (FR-NAS-001). Returns "" with
// no error when no mapping exists — the caller (a vendor AttributeBuilder)
// is responsible for treating an empty profile name as a build error, so
// the nas_attribute_build_errors_total metric fires exactly once, at the
// point that actually knows it's a problem.
func (s *NASStore) GetPlanNASProfile(ctx context.Context, planID int, vendor string) (string, error) {
	const q = `
		SELECT profile_name FROM plan_nas_profiles
		WHERE plan_id = $1 AND vendor = $2`

	var profileName string
	err := s.pool.QueryRow(ctx, q, planID, vendor).Scan(&profileName)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("db: get plan_nas_profile for plan %d vendor %s: %w", planID, vendor, err)
	}
	return profileName, nil
}
