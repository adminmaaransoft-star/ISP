-- +goose Up
-- Session DB-012 | FR-FRN-002 | DBD §6.2 lco_ledger
-- Must run after 007 (wallet_ledgers) for transaction_ref linkage

CREATE TABLE IF NOT EXISTS lco_ledger (
    id                SERIAL          PRIMARY KEY,
    franchise_id      INTEGER         NOT NULL REFERENCES franchises(id),
    subscriber_id     INTEGER         NOT NULL REFERENCES subscribers(id),
    recharge_amount   NUMERIC(12,2)   NOT NULL,
    commission_amount NUMERIC(12,2)   NOT NULL,
    transaction_ref   VARCHAR(100),                   -- links to wallet_ledgers.transaction_token
    created_at        TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- DBD §6.4 — LCO subscriber isolation index (defined on subscribers, added here for context)
-- Note: The following index is already added in migration 003 via idx_franchise_subscribers.
-- Repeated here as documentation reference per DBD §6.4.

-- +goose Down
DROP TABLE IF EXISTS lco_ledger CASCADE;
