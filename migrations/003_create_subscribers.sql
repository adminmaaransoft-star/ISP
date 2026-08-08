-- +goose Up
-- Session DB-003 | FR-AAA-001..004, FR-BIL-001, FR-FRN-001 | DBD §6.2 subscribers
-- Core entity — must run after 001 (franchises) and 002 (plans)

CREATE TABLE IF NOT EXISTS subscribers (
    id               SERIAL          PRIMARY KEY,
    caf_number       VARCHAR(50)     UNIQUE NOT NULL,
    username         VARCHAR(100)    UNIQUE NOT NULL,
    password_hash    TEXT            NOT NULL,          -- bcrypt cost=12; never plaintext
    mobile_number    VARCHAR(20)     NOT NULL,          -- E.164: +91XXXXXXXXXX
    email            VARCHAR(255),
    plan_id          INTEGER         NOT NULL REFERENCES plans(id),
    franchise_id     INTEGER         REFERENCES franchises(id) ON DELETE SET NULL,
    status           VARCHAR(20)     NOT NULL
                         CHECK (status IN ('active','grace_period','soft_suspended',
                                           'hard_suspended','terminated')),
    dunning_state    VARCHAR(20)     NOT NULL DEFAULT 'active',
    wallet_balance   NUMERIC(12,2)   NOT NULL DEFAULT 0.00,
    ipv4_address     INET,                              -- NULL = dynamic
    registered_state VARCHAR(10)     NOT NULL,          -- ISO state code for GST routing
    dnd_opt_out      BOOLEAN         NOT NULL DEFAULT FALSE,
    kyc_status       VARCHAR(20)     NOT NULL DEFAULT 'pending',
    plan_expiry      TIMESTAMPTZ,
    created_at       TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- updated_at trigger.
-- StatementBegin/End are required: goose splits migrations on semicolons, and
-- without these markers it would cut this function body in half at the first
-- internal ';' and fail to apply.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER trg_subscribers_updated_at
    BEFORE UPDATE ON subscribers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- DBD §6.4 indexes
CREATE INDEX idx_sub_auth    ON subscribers(username, status);
CREATE INDEX idx_sub_expiry  ON subscribers(plan_expiry, status)
    WHERE status IN ('active','grace_period');
CREATE INDEX idx_revenue_unbilled ON subscribers(status, plan_expiry)
    WHERE status = 'active';

-- +goose Down
DROP TRIGGER IF EXISTS trg_subscribers_updated_at ON subscribers;
DROP TABLE IF EXISTS subscribers CASCADE;
DROP FUNCTION IF EXISTS set_updated_at();
