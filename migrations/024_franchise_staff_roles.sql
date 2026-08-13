-- +goose Up
-- Session DB-024 | FR-FRN-004, FR-FRN-005 | MDS §4.10, DBD §6.2 staff_users
--
-- internal/revenue/franchise.go has always recognised three franchise-scoped
-- roles (lco, franchise_admin, franchise_staff) in franchiseScopedRoles, but
-- staff_users.chk_staff_role — added twelve migrations later — permits only
-- the five ISP-wide roles. No account could ever hold a franchise role, so
-- FranchiseMiddleware's scoping branch was unreachable in production and any
-- route gated on those roles would have been permanently inaccessible.
--
-- This widens the constraint to the union of both sets, which is what makes
-- FR-FRN-004's role gating mean anything.
--
-- lco is included alongside franchise_admin/franchise_staff because
-- franchiseScopedRoles already treats all three identically; leaving it out
-- would put the constraint and the middleware right back out of step.

ALTER TABLE staff_users DROP CONSTRAINT IF EXISTS chk_staff_role;

ALTER TABLE staff_users ADD CONSTRAINT chk_staff_role CHECK (
    role IN (
        -- ISP-wide staff (migration 021)
        'isp_owner', 'noc_engineer', 'billing_admin', 'csr', 'technician',
        -- Franchise-scoped (internal/revenue/franchise.go franchiseScopedRoles)
        'lco', 'franchise_admin', 'franchise_staff'
    )
);

-- A franchise-scoped account must say which franchise it belongs to. Without
-- this, such an account authenticates and then fails every scoped request at
-- the middleware ("token has no franchise binding") — a failure that would
-- only ever be discovered by a real LCO partner trying to log in.
ALTER TABLE staff_users
    ADD COLUMN IF NOT EXISTS franchise_id INTEGER;

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_constraint WHERE conname = 'fk_staff_users_franchise') THEN
        ALTER TABLE staff_users ADD CONSTRAINT fk_staff_users_franchise
            FOREIGN KEY (franchise_id) REFERENCES franchises(id);
    END IF;

    -- The binding is required for franchise-scoped roles and meaningless for
    -- ISP-wide ones, so the constraint expresses exactly that rather than
    -- leaving the column nullable for everyone and hoping callers get it right.
    IF NOT EXISTS (SELECT FROM pg_constraint WHERE conname = 'chk_staff_franchise_binding') THEN
        ALTER TABLE staff_users ADD CONSTRAINT chk_staff_franchise_binding CHECK (
            (role IN ('lco', 'franchise_admin', 'franchise_staff') AND franchise_id IS NOT NULL)
            OR
            (role NOT IN ('lco', 'franchise_admin', 'franchise_staff') AND franchise_id IS NULL)
        );
    END IF;
END
$$;
-- +goose StatementEnd

CREATE INDEX IF NOT EXISTS idx_staff_users_franchise ON staff_users (franchise_id)
    WHERE franchise_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_staff_users_franchise;

ALTER TABLE staff_users
    DROP CONSTRAINT IF EXISTS chk_staff_franchise_binding,
    DROP CONSTRAINT IF EXISTS fk_staff_users_franchise;

ALTER TABLE staff_users DROP COLUMN IF EXISTS franchise_id;

-- Any account holding a franchise role would violate the narrowed constraint
-- below, so they are deactivated rather than left to block the rollback.
UPDATE staff_users SET active = FALSE
 WHERE role IN ('lco', 'franchise_admin', 'franchise_staff');
DELETE FROM staff_users
 WHERE role IN ('lco', 'franchise_admin', 'franchise_staff');

ALTER TABLE staff_users DROP CONSTRAINT IF EXISTS chk_staff_role;
ALTER TABLE staff_users ADD CONSTRAINT chk_staff_role CHECK (
    role IN ('isp_owner', 'noc_engineer', 'billing_admin', 'csr', 'technician')
);
