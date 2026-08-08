-- +goose Up
-- Session DB-002 | FR-AAA-004, FR-FUP-001 | DBD §6.2 plans
-- Must run after 001 (franchises FK)

CREATE TABLE IF NOT EXISTS plans (
    id                  SERIAL          PRIMARY KEY,
    name                VARCHAR(100)    NOT NULL,
    rate_limit_string   VARCHAR(50)     NOT NULL,   -- MikroTik format: 100M/100M
    volume_gb           INTEGER         NOT NULL,
    fup_threshold_bytes BIGINT          NOT NULL DEFAULT 0,  -- 0 = unlimited
    fup_throttle_string VARCHAR(50),                -- NULL = no throttle
    price               NUMERIC(12,2)   NOT NULL,
    validity_days       INTEGER         NOT NULL,
    franchise_id        INTEGER         REFERENCES franchises(id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS plans CASCADE;
