package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maaransoft/isp-bss-oss/internal/tr069"
)

// TR069Store serves the ACS. Satisfies tr069.Store.
type TR069Store struct{ pool dbPool }

var _ tr069.Store = (*TR069Store)(nil)

const tr069DeviceColumns = `
	d.id, d.serial_number, COALESCE(d.oui,''), COALESCE(d.product_class,''),
	d.device_type_id, d.subscriber_id, COALESCE(d.connection_request_url,''),
	COALESCE(d.software_version,''), COALESCE(d.hardware_version,''),
	d.last_inform_at, COALESCE(d.last_inform_event,''),
	d.provisioning_state, COALESCE(d.last_fault,''), d.acs_discovered`

func scanTR069Device(row interface{ Scan(dest ...any) error }) (*tr069.Device, error) {
	var d tr069.Device
	err := row.Scan(
		&d.ID, &d.SerialNumber, &d.OUI, &d.ProductClass,
		&d.DeviceTypeID, &d.SubscriberID, &d.ConnectionRequestURL,
		&d.SoftwareVersion, &d.HardwareVersion,
		&d.LastInformAt, &d.LastInformEvent,
		&d.ProvisioningState, &d.LastFault, &d.ACSDiscovered,
	)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// UpsertDeviceFromInform records contact from a device.
//
// A device that Informs without a warehouse record still has to be managed —
// field swaps happen, and refusing to talk to it would leave a live
// subscriber unprovisioned. It is created with acs_discovered=true and a
// non-stock status so FR-INV-003's low-stock counts, which mean "boxes on a
// shelf", never include a router that is plainly in somebody's flat.
func (s *TR069Store) UpsertDeviceFromInform(ctx context.Context, inform *tr069.Inform) (*tr069.Device, error) {
	serial := inform.DeviceID.SerialNumber
	if serial == "" {
		return nil, fmt.Errorf("db: Inform carried no serial number")
	}

	// Parameter paths differ between TR-098 and TR-181, so these are matched
	// on suffix (see Inform.Param).
	softwareVersion := inform.Param(".SoftwareVersion")
	hardwareVersion := inform.Param(".HardwareVersion")
	connReqURL := inform.Param(".ConnectionRequestURL")

	const q = `
		WITH ups AS (
			INSERT INTO cpe_devices (
				device_type_id, serial_number, status, oui, product_class,
				connection_request_url, software_version, hardware_version,
				last_inform_at, last_inform_event, provisioning_state, acs_discovered
			)
			VALUES (
				(SELECT id FROM cpe_device_types ORDER BY id LIMIT 1),
				$1, 'returned', NULLIF($2,''), NULLIF($3,''),
				NULLIF($4,''), NULLIF($5,''), NULLIF($6,''),
				NOW(), $7, 'registered', TRUE
			)
			ON CONFLICT (serial_number) DO UPDATE SET
				oui                    = COALESCE(NULLIF(EXCLUDED.oui,''), cpe_devices.oui),
				product_class          = COALESCE(NULLIF(EXCLUDED.product_class,''), cpe_devices.product_class),
				connection_request_url = COALESCE(NULLIF(EXCLUDED.connection_request_url,''), cpe_devices.connection_request_url),
				software_version       = COALESCE(NULLIF(EXCLUDED.software_version,''), cpe_devices.software_version),
				hardware_version       = COALESCE(NULLIF(EXCLUDED.hardware_version,''), cpe_devices.hardware_version),
				last_inform_at         = NOW(),
				last_inform_event      = EXCLUDED.last_inform_event,
				-- A device still at 'unknown' becomes 'registered' on first
				-- contact; anything further along keeps its state, so a
				-- routine check-in never undoes a completed provisioning.
				provisioning_state     = CASE
					WHEN cpe_devices.provisioning_state = 'unknown' THEN 'registered'
					ELSE cpe_devices.provisioning_state
				END
			RETURNING *
		)
		SELECT ` + tr069DeviceColumns + ` FROM ups d`

	device, err := scanTR069Device(s.pool.QueryRow(ctx, q,
		serial, inform.DeviceID.OUI, inform.DeviceID.ProductClass,
		connReqURL, softwareVersion, hardwareVersion, inform.EventCodes()))
	if err != nil {
		return nil, fmt.Errorf("db: upsert device %q from Inform: %w", serial, err)
	}
	return device, nil
}

// GetDeviceBySerialACS loads a device's ACS view.
func (s *TR069Store) GetDeviceBySerialACS(ctx context.Context, serial string) (*tr069.Device, error) {
	const q = `SELECT ` + tr069DeviceColumns + ` FROM cpe_devices d WHERE d.serial_number = $1`
	d, err := scanTR069Device(s.pool.QueryRow(ctx, q, serial))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("db: get ACS device %q: %w", serial, err)
	}
	return d, nil
}

// SetProvisioningState records where a device has reached.
func (s *TR069Store) SetProvisioningState(ctx context.Context, deviceID int, state, lastFault string) error {
	const q = `UPDATE cpe_devices SET provisioning_state = $2, last_fault = NULLIF($3,'') WHERE id = $1`
	if _, err := s.pool.Exec(ctx, q, deviceID, state, lastFault); err != nil {
		return fmt.Errorf("db: set provisioning state for device %d: %w", deviceID, err)
	}
	return nil
}

// ── Task queue ───────────────────────────────────────────────────────────────

const tr069TaskColumns = `
	id, device_id, rpc_type, params, status, priority, created_by,
	COALESCE(fault_code,''), COALESCE(fault_string,''),
	created_at, sent_at, completed_at`

func scanTR069Task(row interface{ Scan(dest ...any) error }) (*tr069.Task, error) {
	var (
		t      tr069.Task
		params []byte
	)
	err := row.Scan(
		&t.ID, &t.DeviceID, &t.RPCType, &params, &t.Status, &t.Priority, &t.CreatedBy,
		&t.FaultCode, &t.FaultString, &t.CreatedAt, &t.SentAt, &t.CompletedAt,
	)
	if err != nil {
		return nil, err
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &t.Params); err != nil {
			return nil, fmt.Errorf("db: unmarshal task params: %w", err)
		}
	}
	return &t, nil
}

// EnqueueTask queues an RPC for the next session the device opens.
func (s *TR069Store) EnqueueTask(ctx context.Context, deviceID int, rpcType string,
	params map[string]string, priority int, createdBy string,
) (*tr069.Task, error) {
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("db: marshal task params: %w", err)
	}

	const q = `
		INSERT INTO cpe_tasks (device_id, rpc_type, params, priority, created_by)
		VALUES ($1, $2, $3::jsonb, $4, $5)
		RETURNING ` + tr069TaskColumns

	task, err := scanTR069Task(s.pool.QueryRow(ctx, q, deviceID, rpcType, payload, priority, createdBy))
	if err != nil {
		return nil, fmt.Errorf("db: enqueue %s for device %d: %w", rpcType, deviceID, err)
	}
	return task, nil
}

// ClaimNextTask atomically hands out the next pending RPC.
//
// What makes this safe under concurrency is the `status = 'pending'`
// predicate inside the sub-select, not the locking hint. Under READ
// COMMITTED, a second updater that blocks on the row lock re-evaluates the
// outer WHERE when it wakes; the sub-select then matches nothing, the UPDATE
// touches zero rows, and the loser correctly gets (nil, nil). Removing that
// predicate was verified to hand the same reboot to all ten concurrent
// claimers — a second, unexplained outage for the subscriber.
//
// FOR UPDATE SKIP LOCKED is a throughput optimisation on top: it lets a
// concurrent claimer move straight to the next queued task instead of
// blocking and then discovering it lost.
//
// Expired tasks are skipped rather than delivered: a reboot queued a
// fortnight ago arriving now would be an unexplained outage too.
func (s *TR069Store) ClaimNextTask(ctx context.Context, deviceID int) (*tr069.Task, error) {
	const q = `
		UPDATE cpe_tasks SET status = 'sent', sent_at = NOW()
		WHERE id = (
			SELECT id FROM cpe_tasks
			WHERE device_id = $1 AND status = 'pending' AND expires_at > NOW()
			ORDER BY priority, created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING ` + tr069TaskColumns

	task, err := scanTR069Task(s.pool.QueryRow(ctx, q, deviceID))
	if isNoRows(err) {
		return nil, nil // nothing queued
	}
	if err != nil {
		return nil, fmt.Errorf("db: claim next task for device %d: %w", deviceID, err)
	}
	return task, nil
}

// CompleteTask records an RPC's outcome.
func (s *TR069Store) CompleteTask(ctx context.Context, taskID int, status, faultCode, faultString string) error {
	const q = `
		UPDATE cpe_tasks
		SET status = $2, fault_code = NULLIF($3,''), fault_string = NULLIF($4,''), completed_at = NOW()
		WHERE id = $1`

	tag, err := s.pool.Exec(ctx, q, taskID, status, faultCode, faultString)
	if err != nil {
		return fmt.Errorf("db: complete task %d: %w", taskID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("db: task %d: %w", taskID, ErrNotFound)
	}
	return nil
}

// ListTasks returns a device's RPC history, newest first.
func (s *TR069Store) ListTasks(ctx context.Context, deviceID int, status *string) ([]tr069.Task, error) {
	const q = `
		SELECT ` + tr069TaskColumns + `
		FROM cpe_tasks
		WHERE device_id = $1 AND ($2::text IS NULL OR status = $2)
		ORDER BY created_at DESC
		LIMIT 100`

	rows, err := s.pool.Query(ctx, q, deviceID, status)
	if err != nil {
		return nil, fmt.Errorf("db: list tasks for device %d: %w", deviceID, err)
	}
	defer rows.Close()

	var out []tr069.Task
	for rows.Next() {
		t, err := scanTR069Task(rows)
		if err != nil {
			return nil, fmt.Errorf("db: scan task: %w", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate tasks: %w", err)
	}
	return out, nil
}

// GetProvisioningPlan builds the parameter map for a device from its model's
// template and its subscriber's plan (FR-CPE-002).
//
// Returns an empty map — not an error — when the device has no subscriber or
// its model has no template: an unissued warehouse device legitimately has
// nothing to be configured with, and treating that as a failure would make
// every stock device's Inform look broken.
func (s *TR069Store) GetProvisioningPlan(ctx context.Context, deviceID int) (map[string]string, error) {
	const q = `
		SELECT t.provisioning_template,
		       COALESCE(s.username, ''), COALESCE(p.rate_limit_string, ''), COALESCE(p.name, ''),
		       d.serial_number
		FROM cpe_devices d
		JOIN cpe_device_types t ON t.id = d.device_type_id
		LEFT JOIN subscribers s ON s.id = d.subscriber_id
		LEFT JOIN plans p       ON p.id = s.plan_id
		WHERE d.id = $1`

	var (
		templateRaw            []byte
		username, rateLimit    string
		planName, serialNumber string
	)
	err := s.pool.QueryRow(ctx, q, deviceID).Scan(
		&templateRaw, &username, &rateLimit, &planName, &serialNumber)
	if isNoRows(err) {
		return nil, fmt.Errorf("db: device %d: %w", deviceID, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("db: get provisioning plan for device %d: %w", deviceID, err)
	}
	if len(templateRaw) == 0 || username == "" {
		return map[string]string{}, nil
	}

	var template map[string]string
	if err := json.Unmarshal(templateRaw, &template); err != nil {
		return nil, fmt.Errorf("db: unmarshal provisioning template: %w", err)
	}

	upstream, downstream := tr069.RateLimitToKbps(rateLimit)
	return tr069.RenderTemplate(template, tr069.ProvisioningContext{
		PPPoEUsername:  username,
		SSID:           planName,
		RateLimit:      rateLimit,
		UpstreamKbps:   upstream,
		DownstreamKbps: downstream,
		PlanName:       planName,
		SerialNumber:   serialNumber,
	}), nil
}
