-- +goose Up
-- Session DB-022 | FR-NAS-001, FR-NAS-002 | DBD §6.2 nas_devices, plan_nas_profiles
--
-- Vendor list expanded from DBD v2.1's 6 values to 8 during implementation:
-- Cisco WLC (vendor 14179, Airespace-derived attributes), Aruba (vendor
-- 14823) and Ruckus each need their own attribute encoding and cannot share
-- one "wireless_generic" bucket as originally drafted — DBD §6.2 should be
-- corrected to match this migration, not the other way round.
--
-- radius_secret_encrypted follows the exact kyc_verifications pattern
-- (migration 005): {key_version}:{base64(nonce+ct)}, never plaintext.

CREATE TABLE IF NOT EXISTS nas_devices (
    id                       SERIAL        PRIMARY KEY,
    ip                       INET          UNIQUE NOT NULL,
    vendor                   VARCHAR(20)   NOT NULL
        CHECK (vendor IN ('mikrotik','huawei','zte','cisco','juniper','cisco_wlc','aruba','ruckus')),
    description              VARCHAR(100),
    radius_secret_encrypted  TEXT          NOT NULL,
    key_version_id           VARCHAR(10)   NOT NULL REFERENCES encryption_keys(version_id),
    coa_port                 INTEGER       NOT NULL DEFAULT 1700,  -- MikroTik default; RFC 5176 standard is 3799
    pod_port                 INTEGER       NOT NULL DEFAULT 1700,
    created_at               TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS plan_nas_profiles (
    id            SERIAL        PRIMARY KEY,
    plan_id       INTEGER       NOT NULL REFERENCES plans(id),
    vendor        VARCHAR(20)   NOT NULL
        CHECK (vendor IN ('mikrotik','huawei','zte','cisco','juniper','cisco_wlc','aruba','ruckus')),
    profile_name  VARCHAR(100)  NOT NULL,
    created_at    TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_plan_vendor UNIQUE (plan_id, vendor)
);

-- DBD §6.4 — plan-to-profile resolution for reference-model vendors (FR-NAS-001)
CREATE INDEX idx_plan_nas_vendor ON plan_nas_profiles(plan_id, vendor);

-- +goose Down
DROP TABLE IF EXISTS plan_nas_profiles CASCADE;
DROP TABLE IF EXISTS nas_devices CASCADE;
