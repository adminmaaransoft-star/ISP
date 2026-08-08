-- +goose Up
-- Session DB-014 | FR-OBS-003 | DBD §6.2 lea_audit_log + §6.5 row security
-- Append-only via RLS: INSERT allowed, UPDATE/DELETE blocked for application role

CREATE TABLE IF NOT EXISTS lea_audit_log (
    id                    BIGSERIAL       PRIMARY KEY,
    accessor_identity     VARCHAR(255)    NOT NULL,   -- JWT sub claim
    accessor_role         VARCHAR(50)     NOT NULL,
    queried_public_ip     INET            NOT NULL,
    queried_port          INTEGER,
    queried_timestamp     TIMESTAMPTZ     NOT NULL,
    result_subscriber_id  INTEGER,
    result_row_count      INTEGER         NOT NULL,
    accessed_at           TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- DBD §6.5 — row-level security: application role can only INSERT
ALTER TABLE lea_audit_log ENABLE ROW LEVEL SECURITY;

-- Allow inserts from any role (application inserts audit records)
CREATE POLICY lea_insert_only ON lea_audit_log
    FOR INSERT WITH CHECK (true);

-- Deny SELECT/UPDATE/DELETE unless the caller is a superuser or has explicit grant
-- (By default, RLS blocks all access not covered by a policy)

-- +goose Down
DROP POLICY IF EXISTS lea_insert_only ON lea_audit_log;
DROP TABLE IF EXISTS lea_audit_log CASCADE;
