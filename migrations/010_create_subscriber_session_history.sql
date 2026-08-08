-- +goose Up
-- Session DB-010 | FR-NET-001, FR-NET-003 | DBD §6.2 subscriber_session_history
-- RANGE partitioned monthly on start_time.
--
-- Partitioning uses PostgreSQL 15's native declarative partitioning rather than
-- pg_partman. IDD §8.2 pins postgres:15-alpine, which does not ship pg_partman,
-- so the extension version of this migration could never apply to the stack the
-- project actually deploys. Native partitioning needs no extension and no custom
-- image; create_monthly_partitions() below covers the roll-forward that
-- partman.create_parent() was providing.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION create_monthly_partitions(
    parent_table TEXT,
    months_ahead INT DEFAULT 3
) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    first_month  DATE := date_trunc('month', NOW())::date;
    offset_month INT;
    range_start  DATE;
    range_end    DATE;
    part_name    TEXT;
BEGIN
    FOR offset_month IN 0..months_ahead LOOP
        range_start := (first_month + (offset_month     || ' month')::interval)::date;
        range_end   := (first_month + (offset_month + 1 || ' month')::interval)::date;
        part_name   := parent_table || '_' || to_char(range_start, 'YYYY_MM');

        IF NOT EXISTS (
            SELECT 1 FROM pg_class c
            JOIN pg_namespace n ON n.oid = c.relnamespace
            WHERE c.relname = part_name AND n.nspname = 'public'
        ) THEN
            EXECUTE format(
                'CREATE TABLE public.%I PARTITION OF public.%I FOR VALUES FROM (%L) TO (%L)',
                part_name, parent_table, range_start, range_end
            );
        END IF;
    END LOOP;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
COMMENT ON FUNCTION create_monthly_partitions(TEXT, INT) IS
    'Creates monthly RANGE partitions for the current month plus months_ahead. Idempotent. Must be called from the monthly maintenance job (OPS §12) so partitions always exist ahead of writes: an insert with no matching partition fails outright.';
-- +goose StatementEnd

CREATE TABLE IF NOT EXISTS subscriber_session_history (
    id                BIGSERIAL    NOT NULL,
    subscriber_id     INTEGER      NOT NULL REFERENCES subscribers(id),
    session_id        VARCHAR(255) NOT NULL,          -- RADIUS Acct-Session-Id
    nas_ip_address    INET         NOT NULL,
    assigned_ipv4     INET,
    assigned_ipv6_prefix CIDR,
    start_time        TIMESTAMPTZ  NOT NULL,           -- partition key
    stop_time         TIMESTAMPTZ,                     -- NULL = session still active
    input_octets      BIGINT       NOT NULL DEFAULT 0,
    output_octets     BIGINT       NOT NULL DEFAULT 0,
    terminate_cause   VARCHAR(50),
    PRIMARY KEY (id, start_time)
) PARTITION BY RANGE (start_time);

-- Current month + 3 ahead, matching the partman.create_parent(..., 3) this replaces.
SELECT create_monthly_partitions('subscriber_session_history', 3);

-- DBD §6.4 indexes.
-- Declared on the parent: PostgreSQL propagates them to every existing and
-- future partition automatically.

-- LEA IP-to-subscriber lookup (INCLUDE to satisfy index-only scans for LEA queries)
CREATE INDEX idx_lea_ipv4_time ON subscriber_session_history(assigned_ipv4, start_time DESC)
    INCLUDE (subscriber_id, stop_time);

-- Active session cleanup on NAS reconnect
CREATE INDEX idx_nas_active ON subscriber_session_history(nas_ip_address)
    WHERE stop_time IS NULL;

-- Live-session lookup by subscriber: health endpoint, portal dashboard, FUP scan
CREATE INDEX idx_session_active_subscriber ON subscriber_session_history(subscriber_id, start_time DESC)
    WHERE stop_time IS NULL;

-- +goose Down
DROP TABLE IF EXISTS subscriber_session_history CASCADE;
DROP FUNCTION IF EXISTS create_monthly_partitions(TEXT, INT);
