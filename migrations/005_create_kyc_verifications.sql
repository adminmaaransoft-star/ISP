-- +goose Up
-- Session DB-005 | FR-SEC-002, FR-SEC-003 | DBD §6.2 kyc_verifications
-- aadhaar and PAN stored AES-GCM-256 encrypted; never plaintext

CREATE TABLE IF NOT EXISTS kyc_verifications (
    id                SERIAL       PRIMARY KEY,
    subscriber_id     INTEGER      NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    aadhaar_encrypted TEXT,                          -- {key_version}:{base64(nonce+ct)}
    pan_encrypted     TEXT,                          -- {key_version}:{base64(nonce+ct)}
    key_version_id    VARCHAR(10)  NOT NULL REFERENCES encryption_keys(version_id),
    verified_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS kyc_verifications CASCADE;
