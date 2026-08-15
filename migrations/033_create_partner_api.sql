-- +goose Up
-- Session DB-033 | FR-API-001..003 | DBD §6.8 | MDS §4.22
--
-- Partner API keys and outbound webhooks. Entirely new surface: nothing here
-- extends an existing table, because a partner integration is not a staff
-- account and conflating the two is exactly what FR-API-001 exists to prevent.

-- ── 1. API keys — FR-API-001 ─────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS api_keys (
    id            SERIAL PRIMARY KEY,
    partner_name  VARCHAR(100) NOT NULL,

    -- The prefix is what makes lookup possible at all. The key is stored
    -- hashed, so the server cannot search by it; it parses this prefix from
    -- the presented key, fetches that one row and compares — one hash per
    -- request rather than one per stored key. It is also what an operator
    -- sees in the console, since the key itself is shown exactly once.
    key_prefix    VARCHAR(24)  NOT NULL UNIQUE,

    -- SHA-256, not bcrypt. An API key is 256 bits of CSPRNG output, so there
    -- is no dictionary to slow an attacker down and no salt worth adding —
    -- bcrypt would buy nothing and cost ~100ms of work on every partner
    -- request. (Subscriber passwords remain bcrypt: those are human-chosen
    -- and low-entropy, which is the case bcrypt exists for.)
    key_hash      TEXT         NOT NULL,

    scopes        TEXT[]       NOT NULL,
    active        BOOLEAN      NOT NULL DEFAULT TRUE,
    last_used_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    created_by    VARCHAR(100) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    revoked_at    TIMESTAMPTZ,

    -- cardinality(), not array_length(): array_length of an empty array is
    -- NULL and a CHECK passes on NULL, which is how an earlier constraint in
    -- this schema silently admitted the exact row it was written to reject.
    -- A key with no scopes would authenticate and authorise nothing, which is
    -- a configuration mistake worth refusing at write time.
    CONSTRAINT chk_api_key_scoped CHECK (cardinality(scopes) >= 1)
);

-- Partial index: expired and revoked keys are never looked up by prefix on
-- the authentication path, which is the only latency-sensitive query here.
CREATE INDEX IF NOT EXISTS idx_api_keys_active ON api_keys (key_prefix) WHERE active;

-- ── 2. Webhook endpoints — FR-API-002 ────────────────────────────────────────
CREATE TABLE IF NOT EXISTS webhook_endpoints (
    id             SERIAL PRIMARY KEY,
    api_key_id     INTEGER      NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    url            TEXT         NOT NULL,

    -- Encrypted, not hashed. HMAC needs the secret back to sign with it, so
    -- bcrypt/SHA-256 are the wrong tool here even though SHA-256 is right for
    -- api_keys above. Stored as {key_version}:{base64(nonce+ct)} through the
    -- same AES keystore as nas_devices.radius_secret_encrypted (migration 005).
    secret_encrypted TEXT       NOT NULL,
    key_version_id   VARCHAR(10) NOT NULL REFERENCES encryption_keys(version_id),

    events         TEXT[]       NOT NULL,
    active         BOOLEAN      NOT NULL DEFAULT TRUE,
    description    VARCHAR(200),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- HTTPS only. A webhook carries an event about a named subscriber to a
    -- third party; over plain HTTP that is readable and forgeable in transit.
    -- This catches the trivial case — the private-range check that matters for
    -- SSRF has to happen in Go, at registration AND at delivery, because DNS
    -- can be re-pointed in between.
    CONSTRAINT chk_webhook_https  CHECK (url LIKE 'https://%'),
    CONSTRAINT chk_webhook_events CHECK (cardinality(events) >= 1)
);

CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_active
    ON webhook_endpoints (api_key_id) WHERE active;

-- ── 3. Delivery log — FR-API-003 ─────────────────────────────────────────────
-- Asynq does the retrying; this table does the remembering. They are different
-- jobs: the queue knows about the next attempt, and this is what a partner's
-- support ticket gets answered from three weeks later, long after the queue
-- entry is gone.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id               BIGSERIAL PRIMARY KEY,
    endpoint_id      INTEGER      NOT NULL REFERENCES webhook_endpoints(id) ON DELETE CASCADE,

    -- The idempotency key the partner sees, echoed in every retry of the same
    -- event so a timeout that actually succeeded is recognisable on their side
    -- rather than double-processed.
    event_id         UUID         NOT NULL,
    event_type       VARCHAR(64)  NOT NULL,

    -- Stored as sent, not re-derived at debug time: re-deriving would show
    -- today's subscriber rather than the one the partner received.
    -- Thin by policy (decision 2026-08-15) — {event_id, event_type,
    -- entity_id, occurred_at} and nothing more, so no PII lands here and
    -- DPDP retention does not apply to the delivery log.
    payload          JSONB        NOT NULL,

    status           VARCHAR(16)  NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending', 'delivered', 'failed', 'abandoned')),
    attempts         INTEGER      NOT NULL DEFAULT 0,
    response_status  INTEGER,
    -- Truncated by the writer: a partner's 500 page is not our audit log.
    response_excerpt TEXT,
    last_error       TEXT,
    next_attempt_at  TIMESTAMPTZ,
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    delivered_at     TIMESTAMPTZ
);

-- One row per endpoint per event. Without this, an Asynq retry that runs
-- after the worker crashed mid-write would log the same delivery twice and
-- the attempt count would be unusable for diagnosing a flapping partner.
CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_deliveries_event
    ON webhook_deliveries (endpoint_id, event_id);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_pending
    ON webhook_deliveries (next_attempt_at) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_recent
    ON webhook_deliveries (endpoint_id, created_at DESC);

-- ── 4. Privileges ────────────────────────────────────────────────────────────
-- Explicit for the same reason staff_users needed it in migration 021: ALTER
-- DEFAULT PRIVILEGES covers only objects created by the role that set it.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'bss_app') THEN
        GRANT SELECT, INSERT, UPDATE ON api_keys, webhook_endpoints, webhook_deliveries TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE api_keys_id_seq TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE webhook_endpoints_id_seq TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE webhook_deliveries_id_seq TO bss_app;
        -- No DELETE anywhere: keys are revoked, endpoints deactivated, and the
        -- delivery log is an audit trail (FR-API-003). A partner dispute is
        -- settled from rows nobody could quietly remove.
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS idx_webhook_deliveries_recent;
DROP INDEX IF EXISTS idx_webhook_deliveries_pending;
DROP INDEX IF EXISTS idx_webhook_deliveries_event;
DROP TABLE IF EXISTS webhook_deliveries CASCADE;
DROP INDEX IF EXISTS idx_webhook_endpoints_active;
DROP TABLE IF EXISTS webhook_endpoints CASCADE;
DROP INDEX IF EXISTS idx_api_keys_active;
DROP TABLE IF EXISTS api_keys CASCADE;
