-- +goose Up
-- Session DB-001 | FR-FRN-001 | DBD §6.2 franchises
-- Must run first — subscribers and plans FK to franchises

CREATE TABLE IF NOT EXISTS franchises (
    id                  SERIAL          PRIMARY KEY,
    name                VARCHAR(100)    NOT NULL,
    owner_name          VARCHAR(100)    NOT NULL,
    mobile_number       VARCHAR(20)     NOT NULL,
    commission_rate_pct NUMERIC(5,2)    NOT NULL,
    status              VARCHAR(20)     NOT NULL DEFAULT 'active',
    created_at          TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS franchises CASCADE;
