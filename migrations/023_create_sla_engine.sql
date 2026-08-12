-- +goose Up
-- Session DB-023 | FR-SUP-001, FR-SUP-002, FR-SUP-003 | MDS §4.13, DBD §6.2
--
-- Helpdesk & SLA engine. Adds SLA tracking to the existing tickets table and
-- the four lookup/log tables the engine reads and writes.
--
-- Grants: not repeated here. Migration 019's ALTER DEFAULT PRIVILEGES covers
-- tables created by the role that ran it (postgres, per scripts/demo_up.sh),
-- which includes every table below — verified empirically against a live
-- database with has_table_privilege() rather than assumed, the same way
-- migration 022's tables were checked.

-- ── Lookup: category → default priority (FR-SUP-001) ────────────────────────
-- A table rather than a Go switch so an ops team can retune urgency without
-- a deploy, the same reasoning plan_nas_profiles follows (MDS §4.11).
CREATE TABLE IF NOT EXISTS category_priority_defaults (
    category         VARCHAR(50)  PRIMARY KEY
                         CHECK (category IN ('connectivity','billing','plan_change','other')),
    default_priority VARCHAR(20)  NOT NULL
                         CHECK (default_priority IN ('low','medium','high','critical'))
);

INSERT INTO category_priority_defaults (category, default_priority) VALUES
    ('connectivity', 'high'),    -- no service is the most urgent default
    ('billing',      'medium'),
    ('plan_change',  'low'),
    ('other',        'low')
ON CONFLICT (category) DO NOTHING;

-- ── Lookup: SLA targets per (category, priority) (FR-SUP-001) ───────────────
CREATE TABLE IF NOT EXISTS sla_policies (
    id                 SERIAL       PRIMARY KEY,
    category           VARCHAR(50)  NOT NULL
                           CHECK (category IN ('connectivity','billing','plan_change','other')),
    priority           VARCHAR(20)  NOT NULL
                           CHECK (priority IN ('low','medium','high','critical')),
    response_minutes   INTEGER      NOT NULL CHECK (response_minutes > 0),
    resolution_minutes INTEGER      NOT NULL CHECK (resolution_minutes > 0),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_sla_category_priority UNIQUE (category, priority),
    -- Resolution cannot be tighter than response: a ticket cannot be required
    -- to be fully resolved before anyone is required to have looked at it.
    CONSTRAINT chk_sla_resolution_after_response CHECK (resolution_minutes >= response_minutes)
);

-- All 16 (category, priority) combinations are seeded, not just the four
-- category defaults above: staff can override priority to any value during
-- triage (MDS §4.13), and a combination with no row here makes ticket
-- creation fail loudly by design. Leaving 12 of them unseeded would turn
-- that deliberate failure into a routine one.
--
-- Response times are priority-driven. Resolution times are tighter for
-- connectivity at every priority — an outage and a plan-change request are
-- not the same urgency even when both are labelled "medium", which is the
-- reason this table is keyed on the pair rather than on priority alone.
INSERT INTO sla_policies (category, priority, response_minutes, resolution_minutes) VALUES
    ('connectivity', 'critical',   15,   240),   -- 15m /  4h
    ('connectivity', 'high',       60,   480),   --  1h /  8h
    ('connectivity', 'medium',    240,  1440),   --  4h / 24h
    ('connectivity', 'low',       480,  2880),   --  8h / 48h
    ('billing',      'critical',   15,   480),
    ('billing',      'high',       60,  1440),
    ('billing',      'medium',    240,  2880),
    ('billing',      'low',       480,  4320),
    ('plan_change',  'critical',   15,   480),
    ('plan_change',  'high',       60,  1440),
    ('plan_change',  'medium',    240,  2880),
    ('plan_change',  'low',       480,  4320),
    ('other',        'critical',   15,   480),
    ('other',        'high',       60,  1440),
    ('other',        'medium',    240,  2880),
    ('other',        'low',       480,  4320)
ON CONFLICT ON CONSTRAINT uq_sla_category_priority DO NOTHING;

-- ── Routing rules (FR-SUP-003) ──────────────────────────────────────────────
-- Resolves to a role, not an individual: auto-assigning a person needs a
-- workload/availability model nothing in this schema tracks (MDS §4.13).
CREATE TABLE IF NOT EXISTS ticket_routing_rules (
    id             SERIAL       PRIMARY KEY,
    category       VARCHAR(50)               -- NULL = matches any category
                       CHECK (category IS NULL OR category IN ('connectivity','billing','plan_change','other')),
    franchise_id   INTEGER      REFERENCES franchises(id),  -- NULL = matches any franchise
    target_role    VARCHAR(20)  NOT NULL
                       -- Same role enum staff_users.chk_staff_role uses.
                       CHECK (target_role IN ('isp_owner','noc_engineer','billing_admin','csr','technician')),
    priority_order INTEGER      NOT NULL DEFAULT 100,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Seed: category-based defaults only, no franchise-specific rules. Deliberately
-- matches the roles that can actually reach the Support console section today
-- (csr, technician, isp_owner — internal/staffui/staffui.go). Routing a ticket
-- to noc_engineer or billing_admin would put it in a queue those roles cannot
-- open, which is worse than leaving it unrouted.
INSERT INTO ticket_routing_rules (category, franchise_id, target_role, priority_order) VALUES
    ('connectivity', NULL, 'technician', 10),
    ('billing',      NULL, 'csr',        10),
    ('plan_change',  NULL, 'csr',        10),
    ('other',        NULL, 'csr',        20)
ON CONFLICT DO NOTHING;

-- ── SLA event log (FR-SUP-002) ──────────────────────────────────────────────
-- Append-only, same shape as notification_log and lea_audit_log. The UNIQUE
-- constraint is the SLA scanner's idempotency mechanism, not merely a data
-- integrity rule: the scanner inserts and acts only if a row was actually
-- written (MDS §4.13).
CREATE TABLE IF NOT EXISTS sla_events (
    id          BIGSERIAL    PRIMARY KEY,
    ticket_id   INTEGER      NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    event_type  VARCHAR(30)  NOT NULL
                    CHECK (event_type IN ('response_warning','response_breach',
                                          'resolution_warning','resolution_breach')),
    occurred_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_sla_event UNIQUE (ticket_id, event_type)
);

-- ── tickets: SLA, routing and the FK migration 009 promised ─────────────────
ALTER TABLE tickets
    ADD COLUMN IF NOT EXISTS priority              VARCHAR(20) NOT NULL DEFAULT 'medium',
    ADD COLUMN IF NOT EXISTS sla_response_due_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS sla_resolution_due_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS franchise_id          INTEGER,
    ADD COLUMN IF NOT EXISTS routed_role           VARCHAR(20);

-- Added separately from the ADD COLUMN above: a CHECK added inline with
-- IF NOT EXISTS would be silently skipped on a re-run where the column
-- already exists but the constraint does not.
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_constraint WHERE conname = 'chk_tickets_priority') THEN
        ALTER TABLE tickets ADD CONSTRAINT chk_tickets_priority
            CHECK (priority IN ('low','medium','high','critical'));
    END IF;
    IF NOT EXISTS (SELECT FROM pg_constraint WHERE conname = 'chk_tickets_routed_role') THEN
        ALTER TABLE tickets ADD CONSTRAINT chk_tickets_routed_role
            CHECK (routed_role IS NULL OR routed_role IN
                ('isp_owner','noc_engineer','billing_admin','csr','technician'));
    END IF;
    IF NOT EXISTS (SELECT FROM pg_constraint WHERE conname = 'fk_tickets_franchise') THEN
        ALTER TABLE tickets ADD CONSTRAINT fk_tickets_franchise
            FOREIGN KEY (franchise_id) REFERENCES franchises(id);
    END IF;
END
$$;
-- +goose StatementEnd

-- assigned_to: migration 009 declared "FK to admin_users.id added in future
-- migration". That migration was never written and admin_users never existed;
-- the real staff table is staff_users (migration 021, twelve migrations
-- later). This is that FK, twelve migrations late.
--
-- Any pre-existing assigned_to value that does not match a live staff_users
-- row is nulled first — without an FK there has been nothing stopping such
-- a value being written, and a deployment that has one would otherwise fail
-- this migration outright. Nulling is the safe direction: the ticket stays,
-- it simply returns to the unassigned queue.
UPDATE tickets SET assigned_to = NULL
WHERE assigned_to IS NOT NULL
  AND assigned_to NOT IN (SELECT id FROM staff_users);

-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_constraint WHERE conname = 'fk_tickets_assigned_to') THEN
        ALTER TABLE tickets ADD CONSTRAINT fk_tickets_assigned_to
            FOREIGN KEY (assigned_to) REFERENCES staff_users(id);
    END IF;
END
$$;
-- +goose StatementEnd

-- ── Indexes (DBD §6.4) ──────────────────────────────────────────────────────
-- Partial: the SLA scanner only ever looks at tickets still in play, so
-- indexing resolved/closed rows would grow the index without ever serving
-- a scan.
CREATE INDEX IF NOT EXISTS idx_tickets_sla_resolution ON tickets (sla_resolution_due_at)
    WHERE status NOT IN ('resolved','closed');
CREATE INDEX IF NOT EXISTS idx_tickets_sla_response ON tickets (sla_response_due_at)
    WHERE status = 'open';

-- Absent before this migration: every ticket query in the codebase (portal
-- list, staffui lookup by subscriber) was a sequential scan.
CREATE INDEX IF NOT EXISTS idx_tickets_subscriber ON tickets (subscriber_id);

CREATE INDEX IF NOT EXISTS idx_tickets_assigned_to ON tickets (assigned_to)
    WHERE assigned_to IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tickets_franchise ON tickets (franchise_id)
    WHERE franchise_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_sla_events_ticket ON sla_events (ticket_id, occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_routing_rules_lookup
    ON ticket_routing_rules (category, franchise_id, priority_order);

-- +goose Down
DROP INDEX IF EXISTS idx_routing_rules_lookup;
DROP INDEX IF EXISTS idx_sla_events_ticket;
DROP INDEX IF EXISTS idx_tickets_franchise;
DROP INDEX IF EXISTS idx_tickets_assigned_to;
DROP INDEX IF EXISTS idx_tickets_subscriber;
DROP INDEX IF EXISTS idx_tickets_sla_response;
DROP INDEX IF EXISTS idx_tickets_sla_resolution;

ALTER TABLE tickets
    DROP CONSTRAINT IF EXISTS fk_tickets_assigned_to,
    DROP CONSTRAINT IF EXISTS fk_tickets_franchise,
    DROP CONSTRAINT IF EXISTS chk_tickets_routed_role,
    DROP CONSTRAINT IF EXISTS chk_tickets_priority;

ALTER TABLE tickets
    DROP COLUMN IF EXISTS routed_role,
    DROP COLUMN IF EXISTS franchise_id,
    DROP COLUMN IF EXISTS sla_resolution_due_at,
    DROP COLUMN IF EXISTS sla_response_due_at,
    DROP COLUMN IF EXISTS priority;

DROP TABLE IF EXISTS sla_events CASCADE;
DROP TABLE IF EXISTS ticket_routing_rules CASCADE;
DROP TABLE IF EXISTS sla_policies CASCADE;
DROP TABLE IF EXISTS category_priority_defaults CASCADE;
