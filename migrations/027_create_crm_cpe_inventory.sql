-- +goose Up
-- Session DB-027 | FR-CRM-001..003, FR-INV-001..003 | DBD §6.2 leads,
-- cpe_device_types, cpe_devices, cpe_purchases | MDS §4.16
--
-- Two domains, one migration: a lead converting into a subscriber is the
-- same moment a CPE leaves the shelf, and that shared transaction boundary
-- is why they ship together.

-- ── leads (FR-CRM-001..003) ──────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS leads (
    id                     SERIAL          PRIMARY KEY,
    full_name              VARCHAR(200)    NOT NULL,
    mobile_number          VARCHAR(20)     NOT NULL,
    email                  VARCHAR(255),
    source                 VARCHAR(50)     NOT NULL
                               CHECK (source IN ('walk_in','referral','website','campaign','franchise','other')),
    status                 VARCHAR(20)     NOT NULL DEFAULT 'new'
                               CHECK (status IN ('new','contacted','qualified','converted','lost')),
    franchise_id           INTEGER         REFERENCES franchises(id) ON DELETE SET NULL,
    assigned_to_username   VARCHAR(100),
    notes                  TEXT,
    lost_reason            TEXT,
    converted_subscriber_id INTEGER        REFERENCES subscribers(id),
    created_at             TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    converted_at           TIMESTAMPTZ,

    -- Same E.164 defense-in-depth migration 020 applies to subscribers and
    -- franchises: a lead's number becomes a subscriber's number on
    -- conversion, so letting a malformed one in here would just defer the
    -- rejection to the worst possible moment.
    CONSTRAINT chk_leads_mobile_e164 CHECK (mobile_number ~ '^\+[1-9][0-9]{7,14}$'),

    -- A converted lead that points at no subscriber (or an unconverted one
    -- that does) is the exact state FR-CRM-003's conversion rate would
    -- silently miscount, so it is made unstorable rather than merely
    -- avoided in code.
    CONSTRAINT chk_lead_converted_has_subscriber CHECK (
        (status = 'converted' AND converted_subscriber_id IS NOT NULL) OR
        (status <> 'converted' AND converted_subscriber_id IS NULL)
    )
);

CREATE TRIGGER trg_leads_updated_at
    BEFORE UPDATE ON leads
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The pipeline board: open leads by stage, newest first.
CREATE INDEX IF NOT EXISTS idx_leads_status ON leads(status, created_at DESC);
-- FR-CRM-003 reports conversion rate by source.
CREATE INDEX IF NOT EXISTS idx_leads_source ON leads(source, status);
CREATE INDEX IF NOT EXISTS idx_leads_franchise ON leads(franchise_id) WHERE franchise_id IS NOT NULL;

-- ── cpe_device_types (FR-INV-001, FR-INV-003) ────────────────────────────────
CREATE TABLE IF NOT EXISTS cpe_device_types (
    id                SERIAL          PRIMARY KEY,
    name              VARCHAR(100)    NOT NULL UNIQUE,
    vendor            VARCHAR(100)    NOT NULL,
    -- The in-stock count at or below which FR-INV-003's low-stock alert
    -- fires. Per type, because a ₹500 splitter and a ₹4000 router do not
    -- warrant the same reorder point.
    reorder_threshold INTEGER         NOT NULL DEFAULT 5 CHECK (reorder_threshold >= 0),
    created_at        TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- ── cpe_devices (FR-INV-001..002) ────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS cpe_devices (
    id             SERIAL          PRIMARY KEY,
    device_type_id INTEGER         NOT NULL REFERENCES cpe_device_types(id),
    serial_number  VARCHAR(100)    NOT NULL UNIQUE,
    mac_address    VARCHAR(17)     UNIQUE,
    status         VARCHAR(20)     NOT NULL DEFAULT 'in_stock'
                       CHECK (status IN ('in_stock','issued','returned','faulty')),
    location       VARCHAR(100),
    subscriber_id  INTEGER         REFERENCES subscribers(id),
    issued_at      TIMESTAMPTZ,
    notes          TEXT,
    created_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ     NOT NULL DEFAULT NOW(),

    -- "Issued to nobody" and "in stock but assigned to someone" are not
    -- states a warehouse can physically be in; the schema refuses to record
    -- either, which also means the low-stock count can be trusted.
    CONSTRAINT chk_cpe_issued_has_subscriber CHECK (
        (status = 'issued'  AND subscriber_id IS NOT NULL) OR
        (status <> 'issued' AND subscriber_id IS NULL)
    )
);

CREATE TRIGGER trg_cpe_devices_updated_at
    BEFORE UPDATE ON cpe_devices
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Stock counting per type (the low-stock computation) and the "what does
-- this subscriber hold" lookup.
CREATE INDEX IF NOT EXISTS idx_cpe_type_status ON cpe_devices(device_type_id, status);
CREATE INDEX IF NOT EXISTS idx_cpe_subscriber ON cpe_devices(subscriber_id) WHERE subscriber_id IS NOT NULL;

-- ── cpe_purchases (FR-INV-003) ───────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS cpe_purchases (
    id                    SERIAL          PRIMARY KEY,
    device_type_id        INTEGER         NOT NULL REFERENCES cpe_device_types(id),
    -- Recorded per purchase as well as on the type: the same model is
    -- routinely sourced from different distributors at different prices,
    -- and the type's vendor is only the default.
    vendor                VARCHAR(100)    NOT NULL,
    quantity              INTEGER         NOT NULL CHECK (quantity > 0),
    unit_cost             NUMERIC(12,2)   NOT NULL CHECK (unit_cost >= 0),
    invoice_ref           VARCHAR(100),
    purchased_by_username VARCHAR(100)    NOT NULL,
    purchased_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_cpe_purchases_type ON cpe_purchases(device_type_id, purchased_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_cpe_purchases_type;
DROP TABLE IF EXISTS cpe_purchases CASCADE;

DROP TRIGGER IF EXISTS trg_cpe_devices_updated_at ON cpe_devices;
DROP INDEX IF EXISTS idx_cpe_subscriber;
DROP INDEX IF EXISTS idx_cpe_type_status;
DROP TABLE IF EXISTS cpe_devices CASCADE;

DROP TABLE IF EXISTS cpe_device_types CASCADE;

DROP TRIGGER IF EXISTS trg_leads_updated_at ON leads;
DROP INDEX IF EXISTS idx_leads_franchise;
DROP INDEX IF EXISTS idx_leads_source;
DROP INDEX IF EXISTS idx_leads_status;
DROP TABLE IF EXISTS leads CASCADE;
