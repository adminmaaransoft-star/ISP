-- +goose Up
-- Session DB-013 | FR-REV-001, FR-REV-004 | DBD §6.2 revenue_snapshots + collections_forecast

CREATE TABLE IF NOT EXISTS revenue_snapshots (
    id                         SERIAL          PRIMARY KEY,
    snapshot_date              DATE            NOT NULL,
    unbilled_subscriber_count  INTEGER         NOT NULL,
    ledger_variance            NUMERIC(12,2)   NOT NULL,   -- should be 0.00 in steady state
    total_wallet_balance       NUMERIC(14,2)   NOT NULL,
    created_at                 TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS collections_forecast (
    id                  SERIAL          PRIMARY KEY,
    forecast_date       DATE            NOT NULL,           -- date forecast was generated
    forecast_for_date   DATE            NOT NULL,           -- future date being forecast
    expected_renewals   INTEGER         NOT NULL DEFAULT 0,
    at_risk_renewals    INTEGER         NOT NULL DEFAULT 0,
    expected_revenue    NUMERIC(14,2)   NOT NULL DEFAULT 0.00,
    at_risk_revenue     NUMERIC(14,2)   NOT NULL DEFAULT 0.00
);

-- +goose Down
DROP TABLE IF EXISTS collections_forecast CASCADE;
DROP TABLE IF EXISTS revenue_snapshots CASCADE;
