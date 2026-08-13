-- +goose Up
-- Session DB-025 | FR-BIL-008..011, FR-LC-001..003 | DBD §6.2 wallet_ledgers, payment_refunds
-- MDS §4.14 — billing lifecycle: auto-renewal, staff adjustments, refunds.

-- ── 1. wallet_ledgers: two new counter-leg accounts ─────────────────────────
-- Auto-renewal (FR-BIL-009) debits the wallet against the value of the plan
-- consumed, and staff adjustments/refunds (FR-BIL-010..011) move money for
-- reasons that are neither a recharge nor a gateway settlement. Reusing
-- 'subscriber_wallet'/'payment_gateway_clearing' for either would make
-- FR-REV-002's per-account reconciliation unable to distinguish a real
-- top-up from a plan charge or a staff correction.
ALTER TABLE wallet_ledgers
    DROP CONSTRAINT IF EXISTS chk_wallet_account;
ALTER TABLE wallet_ledgers
    ADD CONSTRAINT chk_wallet_account
        CHECK (account IN ('subscriber_wallet', 'payment_gateway_clearing',
                            'revenue_clearing', 'adjustment_clearing'));

-- ── 2. wallet_ledgers.adjusted_by_username ──────────────────────────────────
-- Staff attribution for adjustment/refund legs (FR-BIL-010, FR-LC-003).
-- Nullable: recharge, auto-renewal and every other posting this table already
-- carries has no staff actor and must stay that way.
ALTER TABLE wallet_ledgers
    ADD COLUMN IF NOT EXISTS adjusted_by_username VARCHAR(100);

-- ── 3. subscribers.wallet_balance non-negative backstop ─────────────────────
-- WalletService.Post's application-level balance check is the normal
-- defense against an overdraft; this is what makes one actually impossible
-- under a race between two concurrent debits for the same subscriber
-- (MDS §4.14). A violation here means the whole posting transaction rolls
-- back, so the ledger and the balance can never disagree even in that case.
ALTER TABLE subscribers
    DROP CONSTRAINT IF EXISTS chk_wallet_balance_nonneg;
ALTER TABLE subscribers
    ADD CONSTRAINT chk_wallet_balance_nonneg CHECK (wallet_balance >= 0);

-- ── 4. payment_refunds ───────────────────────────────────────────────────────
-- Tracks the business event of a refund (FR-BIL-011) separately from the
-- wallet_ledgers posting that moves the money: a refund has a status
-- lifecycle a ledger leg has no room to express. This deployment has no live
-- gateway refund API, so every row is written as 'processed' immediately —
-- the 'requested'/'failed' states exist so a future asynchronous gateway
-- refund can use this table without another migration.
CREATE TABLE IF NOT EXISTS payment_refunds (
    id                   SERIAL          PRIMARY KEY,
    subscriber_id        INTEGER         NOT NULL REFERENCES subscribers(id),
    ledger_entry_id       INTEGER         NOT NULL REFERENCES wallet_ledgers(id),
    amount               NUMERIC(12,2)   NOT NULL CHECK (amount > 0),
    reason               TEXT            NOT NULL,
    status               VARCHAR(20)     NOT NULL DEFAULT 'processed'
                             CHECK (status IN ('requested','processed','failed')),
    refunded_by_username VARCHAR(100)    NOT NULL,
    created_at           TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_refunds_subscriber ON payment_refunds(subscriber_id);

-- +goose Down
DROP INDEX IF EXISTS idx_refunds_subscriber;
DROP TABLE IF EXISTS payment_refunds;

ALTER TABLE subscribers DROP CONSTRAINT IF EXISTS chk_wallet_balance_nonneg;

ALTER TABLE wallet_ledgers DROP COLUMN IF EXISTS adjusted_by_username;

ALTER TABLE wallet_ledgers DROP CONSTRAINT IF EXISTS chk_wallet_account;
ALTER TABLE wallet_ledgers
    ADD CONSTRAINT chk_wallet_account
        CHECK (account IN ('subscriber_wallet', 'payment_gateway_clearing'));
