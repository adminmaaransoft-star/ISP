-- +goose Up
-- Staff identity, so the operations console has something to authenticate
-- against.
--
-- Until now staff existed only as a claim inside a JWT minted out-of-band by
-- scripts/gen_jwt. That is workable for load tests and for calling the API
-- from a terminal, but it cannot back a login form: there is no record to
-- check a password against, no way to revoke one person's access, and no
-- identity to attribute an action to beyond whatever the token's subject
-- happened to say.
--
-- The role column carries the same values the API's RBAC middleware already
-- enforces, so the console grants exactly what the API grants and no more —
-- the console is a client of the same authorisation rules, not a second
-- implementation of them.
--
-- FR: FR-SEC-005 | SecD §9.2, §9.3
CREATE TABLE IF NOT EXISTS staff_users (
    id            SERIAL PRIMARY KEY,
    username      VARCHAR(100) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    full_name     VARCHAR(200) NOT NULL,
    role          VARCHAR(50)  NOT NULL,
    -- lea_access stays a separate column rather than a role, mirroring the JWT
    -- claim: SecD §9.3 requires that reach over law-enforcement lookups can
    -- never be granted as a side effect of assigning somebody a job title.
    lea_access    BOOLEAN      NOT NULL DEFAULT FALSE,
    -- Deactivating rather than deleting keeps the audit trail of what a
    -- departed operator did, which is the point of recording an accessor at
    -- all.
    active        BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ,

    CONSTRAINT chk_staff_role CHECK (
        role IN ('isp_owner', 'noc_engineer', 'billing_admin', 'csr', 'technician')
    )
);

CREATE INDEX IF NOT EXISTS idx_staff_users_active ON staff_users (username) WHERE active;

-- Migration 019 set ALTER DEFAULT PRIVILEGES for bss_app, but those apply only
-- to tables created by the role that ran that statement. Granting explicitly
-- here means this table works regardless of which role applied the migration.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'bss_app') THEN
        GRANT SELECT, INSERT, UPDATE ON staff_users TO bss_app;
        GRANT USAGE, SELECT ON SEQUENCE staff_users_id_seq TO bss_app;
        -- No DELETE: accounts are deactivated, never removed.
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS staff_users;
