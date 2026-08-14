-- +goose Up
-- Session DB-030 | FR-CPE-001..003 | DBD §6.2 cpe_devices (ACS columns),
-- cpe_tasks, cpe_device_types.provisioning_template | MDS §4.19
--
-- A minimal TR-069 ACS. CWMP is CPE-initiated: the ACS cannot push, it can
-- only answer a session the device opens. Behind CGNAT a Connection Request
-- often cannot reach the device at all, so the engine is built around
-- periodic Inform sessions (1 BOOT / 2 PERIODIC) draining a durable queue,
-- with Connection Request as a best-effort accelerator rather than the
-- mechanism.

-- ── 1. ACS state on the device row ───────────────────────────────────────────
-- Extends cpe_devices rather than adding a parallel table: the box in the
-- warehouse and the box talking CWMP are the same physical unit, and
-- serial_number is already its unique identity.
ALTER TABLE cpe_devices
    ADD COLUMN IF NOT EXISTS oui                        VARCHAR(6),
    ADD COLUMN IF NOT EXISTS product_class              VARCHAR(64),
    ADD COLUMN IF NOT EXISTS connection_request_url     TEXT,
    ADD COLUMN IF NOT EXISTS software_version           VARCHAR(64),
    ADD COLUMN IF NOT EXISTS hardware_version           VARCHAR(64),
    ADD COLUMN IF NOT EXISTS last_inform_at             TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_inform_event          VARCHAR(32),
    ADD COLUMN IF NOT EXISTS provisioning_state         VARCHAR(24) NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS last_fault                 TEXT,
    -- Set when a device first Informs without a warehouse record. Such a row
    -- is a real device we must still manage, but it never passed through
    -- stock control — conflating the two would corrupt FR-INV-003's counts.
    ADD COLUMN IF NOT EXISTS acs_discovered             BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE cpe_devices
    DROP CONSTRAINT IF EXISTS chk_cpe_provisioning_state;
ALTER TABLE cpe_devices
    ADD CONSTRAINT chk_cpe_provisioning_state CHECK (
        provisioning_state IN ('unknown','registered','provisioned','needs_reprovision','fault')
    );

-- An ACS-discovered device is not warehouse stock, so it must not carry a
-- stock status that the low-stock count would include.
ALTER TABLE cpe_devices
    DROP CONSTRAINT IF EXISTS chk_cpe_discovered_not_in_stock;
ALTER TABLE cpe_devices
    ADD CONSTRAINT chk_cpe_discovered_not_in_stock CHECK (
        NOT acs_discovered OR status <> 'in_stock'
    );

-- "Which devices have gone quiet" is the ACS's primary health question.
CREATE INDEX IF NOT EXISTS idx_cpe_last_inform ON cpe_devices(last_inform_at DESC NULLS LAST);
CREATE INDEX IF NOT EXISTS idx_cpe_provisioning_state ON cpe_devices(provisioning_state)
    WHERE provisioning_state IN ('needs_reprovision','fault');

-- ── 2. Provisioning template per device model (FR-CPE-001..002) ──────────────
-- TR-069 parameter paths differ wildly between models, so the paths live in
-- data rather than code: a JSONB map of parameter path -> value template,
-- with {{placeholders}} the engine substitutes from the subscriber and plan.
-- A new router model is then a row, not a release.
ALTER TABLE cpe_device_types
    ADD COLUMN IF NOT EXISTS provisioning_template JSONB;

-- ── 3. cpe_tasks: the durable RPC queue (FR-CPE-003) ─────────────────────────
-- Queued rather than executed inline because the ACS cannot reach the device
-- on demand. A task waits until the CPE opens its next session, which behind
-- CGNAT may be its next periodic Inform.
CREATE TABLE IF NOT EXISTS cpe_tasks (
    id             SERIAL       PRIMARY KEY,
    device_id      INTEGER      NOT NULL REFERENCES cpe_devices(id) ON DELETE CASCADE,
    rpc_type       VARCHAR(32)  NOT NULL
                       CHECK (rpc_type IN ('SetParameterValues','GetParameterValues','Reboot','Download','FactoryReset')),
    -- RPC arguments: the parameter map for Set/Get, the firmware URL for
    -- Download. JSONB so one queue serves every RPC shape.
    params         JSONB        NOT NULL DEFAULT '{}'::JSONB,
    status         VARCHAR(20)  NOT NULL DEFAULT 'pending'
                       CHECK (status IN ('pending','sent','completed','failed','expired')),
    -- 'sent' is the claim that stops one task being issued twice when two
    -- sessions overlap, the same conditional-update pattern used for
    -- approvals (MDS §4.15) and lead conversion (§4.16).
    priority       INTEGER      NOT NULL DEFAULT 100,
    created_by     VARCHAR(100) NOT NULL,
    fault_code     VARCHAR(16),
    fault_string   TEXT,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    sent_at        TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    -- A task nobody ever collected should not queue forever: a device that
    -- never comes back would otherwise accumulate an unbounded backlog that
    -- all fires at once if it ever does.
    expires_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW() + INTERVAL '7 days'
);

-- The session hot path: "what is waiting for this device", best first.
CREATE INDEX IF NOT EXISTS idx_cpe_tasks_pending
    ON cpe_tasks(device_id, priority, created_at)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_cpe_tasks_device ON cpe_tasks(device_id, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_cpe_tasks_device;
DROP INDEX IF EXISTS idx_cpe_tasks_pending;
DROP TABLE IF EXISTS cpe_tasks CASCADE;

ALTER TABLE cpe_device_types DROP COLUMN IF EXISTS provisioning_template;

DROP INDEX IF EXISTS idx_cpe_provisioning_state;
DROP INDEX IF EXISTS idx_cpe_last_inform;
ALTER TABLE cpe_devices DROP CONSTRAINT IF EXISTS chk_cpe_discovered_not_in_stock;
ALTER TABLE cpe_devices DROP CONSTRAINT IF EXISTS chk_cpe_provisioning_state;
ALTER TABLE cpe_devices
    DROP COLUMN IF EXISTS acs_discovered,
    DROP COLUMN IF EXISTS last_fault,
    DROP COLUMN IF EXISTS provisioning_state,
    DROP COLUMN IF EXISTS last_inform_event,
    DROP COLUMN IF EXISTS last_inform_at,
    DROP COLUMN IF EXISTS hardware_version,
    DROP COLUMN IF EXISTS software_version,
    DROP COLUMN IF EXISTS connection_request_url,
    DROP COLUMN IF EXISTS product_class,
    DROP COLUMN IF EXISTS oui;
