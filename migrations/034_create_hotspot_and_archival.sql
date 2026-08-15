-- +goose Up
-- Session DB-034 | FR-HSP-001..003, FR-DOC-001 | DBD §6.9 | MDS §4.23, §4.24
--
-- Hotspot access (captive portal, MAC auth bypass, vouchers) and document
-- archival to external storage.

-- ── 1. MAC Auth Bypass is opt-in per NAS — FR-HSP-002 ────────────────────────
--
-- A MAC address is trivially spoofable: it travels in the clear on every frame
-- and any client can set its own. MAB therefore grants service to whoever
-- learns a MAC, which is acceptable on a café hotspot the operator chose to
-- run that way and unacceptable as a global fallback on a PPPoE base.
--
-- DEFAULT FALSE is the load-bearing part of this column. An operator turns MAB
-- on for the one NAS that needs it; every other NAS keeps rejecting a
-- password-less Access-Request exactly as it does today. Making this global —
-- or defaulting it true — would mean any subscriber's service could be taken
-- by anyone who read their MAC off the air.
ALTER TABLE nas_devices
    ADD COLUMN IF NOT EXISTS allow_mab BOOLEAN NOT NULL DEFAULT FALSE;

COMMENT ON COLUMN nas_devices.allow_mab IS
    'FR-HSP-002: permit MAC-address-only authentication from this NAS. Default '
    'FALSE — a MAC is spoofable, so this is per-NAS opt-in, never a global fallback.';

-- ── 2. Hotspot devices — FR-HSP-002 ──────────────────────────────────────────
-- The MAC → subscriber binding MAB authenticates against. Separate from
-- cpe_devices: that table is warehouse stock the operator issued, this is
-- whatever phone or laptop a subscriber walked in with.
CREATE TABLE IF NOT EXISTS hotspot_devices (
    id             SERIAL PRIMARY KEY,
    mac_address    VARCHAR(17)  NOT NULL UNIQUE,
    subscriber_id  INTEGER      NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    label          VARCHAR(100),
    -- Bound at registration and re-checked at auth. A device registered on the
    -- café's NAS must not authenticate on a different operator's hotspot just
    -- because both have MAB enabled.
    nas_id         INTEGER      REFERENCES nas_devices(id) ON DELETE SET NULL,
    active         BOOLEAN      NOT NULL DEFAULT TRUE,
    first_seen_at  TIMESTAMPTZ,
    last_seen_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- Normalised uppercase-with-colons, so 'aa:bb:...' and 'AA:BB:...' cannot
    -- both exist and authenticate as different devices.
    CONSTRAINT chk_hotspot_mac_format
        CHECK (mac_address ~ '^[0-9A-F]{2}(:[0-9A-F]{2}){5}$')
);

CREATE INDEX IF NOT EXISTS idx_hotspot_devices_lookup
    ON hotspot_devices (mac_address) WHERE active;

-- ── 3. Vouchers — FR-HSP-001 ─────────────────────────────────────────────────
-- Prepaid access codes for walk-up users who have no subscriber account.
CREATE TABLE IF NOT EXISTS hotspot_vouchers (
    id              SERIAL PRIMARY KEY,
    -- Stored hashed for the same reason API keys are (migration 033): a
    -- voucher code is a bearer credential, and a printed batch that leaks from
    -- the database is a batch of free service.
    code_hash       TEXT         NOT NULL UNIQUE,
    code_prefix     VARCHAR(12)  NOT NULL,
    plan_id         INTEGER      NOT NULL REFERENCES plans(id),
    franchise_id    INTEGER      REFERENCES franchises(id) ON DELETE SET NULL,
    duration_minutes INTEGER     NOT NULL CHECK (duration_minutes > 0),
    data_cap_bytes  BIGINT       NOT NULL DEFAULT 0 CHECK (data_cap_bytes >= 0),

    status          VARCHAR(16)  NOT NULL DEFAULT 'unused'
                    CHECK (status IN ('unused', 'active', 'used', 'expired', 'void')),
    -- Set on redemption. The MAC that redeemed it, so a single-use voucher
    -- cannot be shared around a room.
    redeemed_by_mac VARCHAR(17),
    redeemed_at     TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    valid_until     TIMESTAMPTZ,

    batch_ref       VARCHAR(64),
    created_by      VARCHAR(100) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- A redeemed voucher must carry both facts or neither. Without this a
    -- half-written redemption leaves a voucher that looks used but cannot be
    -- attributed, which is exactly the row a dispute turns on.
    CONSTRAINT chk_voucher_redemption_complete
        CHECK ((status IN ('unused','void','expired')) OR
               (redeemed_by_mac IS NOT NULL AND redeemed_at IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_hotspot_vouchers_unused
    ON hotspot_vouchers (code_prefix) WHERE status = 'unused';
CREATE INDEX IF NOT EXISTS idx_hotspot_vouchers_batch
    ON hotspot_vouchers (batch_ref, created_at DESC);

-- ── 4. Captive-portal grants — FR-HSP-001 ────────────────────────────────────
-- A short-lived permission to use the network, created when someone completes
-- the walled-garden login and consumed by the NAS when it authenticates them.
CREATE TABLE IF NOT EXISTS hotspot_grants (
    id             BIGSERIAL PRIMARY KEY,
    mac_address    VARCHAR(17)  NOT NULL,
    subscriber_id  INTEGER      REFERENCES subscribers(id) ON DELETE CASCADE,
    voucher_id     INTEGER      REFERENCES hotspot_vouchers(id) ON DELETE SET NULL,
    nas_id         INTEGER      REFERENCES nas_devices(id) ON DELETE SET NULL,
    plan_id        INTEGER      NOT NULL REFERENCES plans(id),

    granted_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at     TIMESTAMPTZ  NOT NULL,
    revoked_at     TIMESTAMPTZ,

    -- A grant is either a subscriber's or a voucher's, never both and never
    -- neither: the rate limit and the accounting have to attach to something.
    CONSTRAINT chk_grant_has_exactly_one_source
        CHECK ((subscriber_id IS NULL) <> (voucher_id IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_hotspot_grants_live
    ON hotspot_grants (mac_address, expires_at DESC) WHERE revoked_at IS NULL;

-- ── 5. Document archival — FR-DOC-001 ────────────────────────────────────────
CREATE TABLE IF NOT EXISTS document_archives (
    id             BIGSERIAL PRIMARY KEY,
    doc_kind       VARCHAR(32)  NOT NULL CHECK (doc_kind IN ('invoice', 'kyc_document', 'report')),
    entity_id      INTEGER      NOT NULL,

    storage_backend VARCHAR(16) NOT NULL CHECK (storage_backend IN ('s3', 'sftp', 'local')),
    storage_url    TEXT         NOT NULL,
    -- SHA-256 of the bytes as uploaded. Archival without a checksum is a copy
    -- nobody can prove is intact; with one, a restore can be verified rather
    -- than hoped over.
    checksum_sha256 CHAR(64)    NOT NULL,
    size_bytes     BIGINT       NOT NULL CHECK (size_bytes > 0),

    archived_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    -- Retention is recorded per document rather than assumed globally: GST
    -- invoices and KYC scans have different statutory minimums in India, and a
    -- single sweep interval would either delete invoices too early or keep KYC
    -- scans far longer than the DPDP principle of storage limitation allows.
    retain_until   TIMESTAMPTZ,
    purged_at      TIMESTAMPTZ,

    CONSTRAINT chk_archive_not_purged_before_retention
        CHECK (purged_at IS NULL OR retain_until IS NULL OR purged_at >= retain_until)
);

-- One archive row per document per backend. A retry after a network failure
-- must not create a second row claiming a second copy that does not exist.
CREATE UNIQUE INDEX IF NOT EXISTS idx_document_archives_unique
    ON document_archives (doc_kind, entity_id, storage_backend) WHERE purged_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_document_archives_due
    ON document_archives (retain_until) WHERE purged_at IS NULL;

-- ── 6. Privileges ────────────────────────────────────────────────────────────
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'bss_app') THEN
        GRANT SELECT, INSERT, UPDATE ON hotspot_devices, hotspot_vouchers,
                                        hotspot_grants, document_archives TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE hotspot_devices_id_seq TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE hotspot_vouchers_id_seq TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE hotspot_grants_id_seq TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE document_archives_id_seq TO bss_app;
        -- No DELETE: vouchers are voided, grants revoked, archives purged by
        -- setting purged_at. Each keeps its audit trail.
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS idx_document_archives_due;
DROP INDEX IF EXISTS idx_document_archives_unique;
DROP TABLE IF EXISTS document_archives CASCADE;
DROP INDEX IF EXISTS idx_hotspot_grants_live;
DROP TABLE IF EXISTS hotspot_grants CASCADE;
DROP INDEX IF EXISTS idx_hotspot_vouchers_batch;
DROP INDEX IF EXISTS idx_hotspot_vouchers_unused;
DROP TABLE IF EXISTS hotspot_vouchers CASCADE;
DROP INDEX IF EXISTS idx_hotspot_devices_lookup;
DROP TABLE IF EXISTS hotspot_devices CASCADE;
ALTER TABLE nas_devices DROP COLUMN IF EXISTS allow_mab;
