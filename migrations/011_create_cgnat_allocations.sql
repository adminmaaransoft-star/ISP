-- +goose Up
-- Session DB-011 | FR-NET-001, FR-NET-002 | DBD §6.2 cgnat_allocations
-- RANGE partitioned monthly on allocated_at, using native declarative
-- partitioning and create_monthly_partitions() from migration 010.

CREATE TABLE IF NOT EXISTS cgnat_allocations (
    id             BIGSERIAL    NOT NULL,
    subscriber_id  INTEGER      NOT NULL REFERENCES subscribers(id),
    public_ip      INET         NOT NULL,
    port_start     INTEGER      NOT NULL,
    port_end       INTEGER      NOT NULL,
    nas_ip_address INET         NOT NULL,
    allocated_at   TIMESTAMPTZ  NOT NULL,             -- partition key
    released_at    TIMESTAMPTZ,
    PRIMARY KEY (id, allocated_at)
) PARTITION BY RANGE (allocated_at);

-- Current month + 3 ahead
SELECT create_monthly_partitions('cgnat_allocations', 3);

-- DBD §6.4 indexes

-- LEA CGNAT port-block lookup with INCLUDE for index-only scan
CREATE INDEX idx_cgnat_lea ON cgnat_allocations(public_ip, allocated_at DESC)
    INCLUDE (subscriber_id, port_start, port_end, released_at);

-- +goose Down
DROP TABLE IF EXISTS cgnat_allocations CASCADE;
