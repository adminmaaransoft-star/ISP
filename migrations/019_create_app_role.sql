-- +goose Up
-- Session DB-019 | DoD Phase 1 Step 2 | SecD §9.7, DBD §6.5
--
-- The application has always connected as the `postgres` superuser (see
-- docker-compose.yml's DB_DSN), which is also the owner of every table here.
-- PostgreSQL row-level security always exempts both superusers and table
-- owners — FORCE ROW LEVEL SECURITY does not even help against a literal
-- superuser — so lea_audit_log's "append-only via RLS" design (migration
-- 014) was never actually enforced against the role the app runs as; an
-- UPDATE/DELETE from the app's own connection silently succeeded. This
-- migration creates a dedicated, non-superuser application role that RLS
-- genuinely applies to, and grants it exactly what the application needs.
--
-- The role's LOGIN password is deliberately NOT set here (this file is
-- committed to git): it is set separately via `ALTER ROLE ... PASSWORD`
-- from an environment variable at bring-up time (see scripts/demo_up.sh),
-- so the real secret never enters version control.

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'bss_app') THEN
        CREATE ROLE bss_app LOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT CONNECT ON DATABASE isp_bss_oss TO bss_app;
GRANT USAGE ON SCHEMA public TO bss_app;

-- Broad DML on every application table and sequence...
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO bss_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO bss_app;

-- ...and the same for anything created after this migration runs. Verified
-- empirically (not assumed) that a GRANT on a partitioned parent table
-- propagates to partitions created later — subscriber_session_history and
-- cgnat_allocations gain new monthly partitions via create_monthly_partitions()
-- long after this migration applies, and this default-privilege grant covers
-- both those and any future migration's new tables without needing the
-- partition-creation function itself to re-grant anything.
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO bss_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO bss_app;

-- lea_audit_log (DBD §6.5, migration 014): the application only ever INSERTs
-- an audit row (internal/db/fup.go RecordLEAAudit) and never reads, updates,
-- or deletes it. Narrowing the broad grant above down to INSERT-only means an
-- UPDATE/DELETE now fails with a clean "permission denied for table" before
-- RLS is even evaluated. The existing lea_insert_only RLS policy (INSERT
-- only — no UPDATE/DELETE/SELECT policy exists) is defense-in-depth on top of
-- that: even a future accidental re-grant of UPDATE/DELETE would still be
-- blocked by RLS's default-deny, since bss_app is neither the table owner
-- nor a superuser.
REVOKE SELECT, UPDATE, DELETE ON lea_audit_log FROM bss_app;

-- goose_db_version: migration bookkeeping the application has no legitimate
-- reason to read or write at runtime.
REVOKE ALL ON goose_db_version FROM bss_app;

-- +goose Down
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM bss_app;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM bss_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM bss_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE USAGE, SELECT ON SEQUENCES FROM bss_app;
REVOKE USAGE ON SCHEMA public FROM bss_app;
REVOKE CONNECT ON DATABASE isp_bss_oss FROM bss_app;
DROP ROLE IF EXISTS bss_app;
