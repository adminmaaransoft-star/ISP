-- +goose Up
-- Session DB-016 | FR-FUP-001, FR-REV-002, FR-SEC-002 | DBD §6.2 corrections
--
-- Three columns/constraints the application code requires that no earlier
-- migration created. Each was found by implementing the persistence layer
-- against the interfaces the domain packages already declare.

-- ── 1. subscribers.fup_active ────────────────────────────────────────────────
-- fup.FUPQuerier.SetFUPActive persists the throttled flag and
-- radius.Subscriber.FUPActive reads it to pick the throttled rate limit for an
-- Access-Accept. Without this column FR-FUP-001 has nowhere to record that a
-- subscriber is throttled, so every scanner pass would re-detect the same breach
-- and re-issue CoA forever.
ALTER TABLE subscribers
    ADD COLUMN IF NOT EXISTS fup_active BOOLEAN NOT NULL DEFAULT FALSE;

-- Scanner reads only breaching, not-yet-throttled subscribers.
CREATE INDEX IF NOT EXISTS idx_sub_fup_active ON subscribers(fup_active)
    WHERE fup_active = FALSE;

-- ── 2. wallet_ledgers.account ────────────────────────────────────────────────
-- A recharge posts two legs against the same subscriber_id: a credit to the
-- subscriber's wallet and the matching debit to the gateway clearing account.
-- Without an account label, SUM(credits) - SUM(debits) over wallet_ledgers is
-- zero by construction, which would make the FR-REV-002 variance check
-- structurally incapable of detecting a discrepancy.
ALTER TABLE wallet_ledgers
    ADD COLUMN IF NOT EXISTS account VARCHAR(40) NOT NULL DEFAULT 'subscriber_wallet';

ALTER TABLE wallet_ledgers
    DROP CONSTRAINT IF EXISTS chk_wallet_account;
ALTER TABLE wallet_ledgers
    ADD CONSTRAINT chk_wallet_account
        CHECK (account IN ('subscriber_wallet', 'payment_gateway_clearing'));

-- Variance reconciliation sums the subscriber_wallet leg per subscriber.
CREATE INDEX IF NOT EXISTS idx_wallet_account_subscriber
    ON wallet_ledgers(account, subscriber_id);

-- ── 3. kyc_verifications uniqueness ──────────────────────────────────────────
-- api.KYCQuerier.UpsertKYC is an upsert, which needs a unique key to conflict
-- on. Without it a subscriber re-submitting KYC accumulates duplicate rows and
-- no query can say which ciphertext is current.
DELETE FROM kyc_verifications a
    USING kyc_verifications b
    WHERE a.subscriber_id = b.subscriber_id AND a.id < b.id;

ALTER TABLE kyc_verifications
    DROP CONSTRAINT IF EXISTS uq_kyc_subscriber;
ALTER TABLE kyc_verifications
    ADD CONSTRAINT uq_kyc_subscriber UNIQUE (subscriber_id);

-- +goose Down
ALTER TABLE kyc_verifications DROP CONSTRAINT IF EXISTS uq_kyc_subscriber;
DROP INDEX IF EXISTS idx_wallet_account_subscriber;
ALTER TABLE wallet_ledgers DROP CONSTRAINT IF EXISTS chk_wallet_account;
ALTER TABLE wallet_ledgers DROP COLUMN IF EXISTS account;
DROP INDEX IF EXISTS idx_sub_fup_active;
ALTER TABLE subscribers DROP COLUMN IF EXISTS fup_active;
