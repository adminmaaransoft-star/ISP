-- +goose Up
-- Session DB-004 | FR-SEC-002, FR-SEC-003 | DBD §6.2 encryption_keys
-- Stores key metadata only — key material NEVER stored in DB (in secret manager)

CREATE TABLE IF NOT EXISTS encryption_keys (
    id           SERIAL       PRIMARY KEY,
    version_id   VARCHAR(10)  UNIQUE NOT NULL,  -- e.g. v1, v2, v3
    key_hash     VARCHAR(64)  NOT NULL,          -- SHA-256 of key material for audit
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    rotated_at   TIMESTAMPTZ,
    status       VARCHAR(10)  NOT NULL DEFAULT 'active'
                     CHECK (status IN ('active','retired'))
);

-- Seed the default v1 key record (key_hash is a placeholder — replace with real SHA-256 on first deploy)
INSERT INTO encryption_keys (version_id, key_hash, status)
VALUES ('v1', 'replace_with_sha256_of_actual_key_material_on_deploy', 'active')
ON CONFLICT (version_id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS encryption_keys CASCADE;
