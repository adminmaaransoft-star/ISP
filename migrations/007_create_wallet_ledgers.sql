-- +goose Up
-- Session DB-007 | FR-BIL-003, FR-BIL-005, FR-REV-002 | DBD §6.2 wallet_ledgers

CREATE TABLE IF NOT EXISTS wallet_ledgers (
    id                SERIAL          PRIMARY KEY,
    subscriber_id     INTEGER         NOT NULL REFERENCES subscribers(id),
    franchise_id      INTEGER         REFERENCES franchises(id) ON DELETE SET NULL,
    entry_type        VARCHAR(20)     NOT NULL CHECK (entry_type IN ('credit','debit')),
    amount            NUMERIC(12,2)   NOT NULL,
    balance_after     NUMERIC(12,2)   NOT NULL,
    transaction_token VARCHAR(100),                   -- NULL for cash payments (no idempotency key)
    description       TEXT,
    created_at        TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- DBD §6.4 — partial unique index: idempotency on non-null tokens only
CREATE UNIQUE INDEX idx_wallet_token ON wallet_ledgers(transaction_token)
    WHERE transaction_token IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS wallet_ledgers CASCADE;
