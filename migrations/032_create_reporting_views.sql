-- +goose Up
-- Session DB-032 | FR-RPT-001, FR-RPT-003 | DBD §6.7 | MDS §4.8 (extended)
--
-- The reporting layer proper. Three plain views and one materialised view,
-- reading the transition history migration 031 began capturing plus the
-- current-state tables that could always answer for themselves.
--
-- Plain views are the default here. They are always current, need no refresh
-- story, and at this data volume cost nothing worth optimising. Only
-- mv_ticket_resolution is materialised, because it computes a percentile
-- across every ticket ever filed and that is not a per-page-load query.
--
-- Area = franchise (decision 2026-08-15, FR-RPT-003). No address, region or
-- pincode column exists anywhere in the schema; franchise territory is the
-- only grouping that maps to real geography today.

-- ── 1. Plan mix — FR-RPT-001 ─────────────────────────────────────────────────
-- Current state only, and legitimately so: "what is our plan mix" is a
-- question about now, not about history, so this one needed no capture.
CREATE OR REPLACE VIEW v_plan_mix AS
SELECT p.id                                                   AS plan_id,
       p.name                                                 AS plan_name,
       p.price,
       s.franchise_id,
       count(*)                                               AS total_subscribers,
       count(*) FILTER (WHERE s.status = 'active')            AS active_subscribers,
       count(*) FILTER (WHERE s.status IN ('soft_suspended',
                                           'hard_suspended')) AS suspended_subscribers,
       -- Monthly recurring revenue counts only accounts that are actually
       -- serviceable. Including suspended ones would report revenue the
       -- business is currently not collecting.
       (p.price * count(*) FILTER (WHERE s.status = 'active'))::NUMERIC(14, 2) AS mrr
  FROM plans p
  JOIN subscribers s ON s.plan_id = p.id
 GROUP BY p.id, p.name, p.price, s.franchise_id;

-- ── 2. Growth and churn — FR-RPT-001 ─────────────────────────────────────────
-- Reads only real events. is_baseline rows are the snapshot migration 031
-- seeded for accounts that predate capture; counting one as a signup would
-- draw a growth curve nobody observed.
CREATE OR REPLACE VIEW v_subscriber_growth_monthly AS
SELECT date_trunc('month', h.occurred_at)                        AS month,
       h.franchise_id,
       h.plan_id,
       count(*) FILTER (WHERE h.old_status IS NULL)              AS new_connections,
       count(*) FILTER (WHERE h.new_status = 'active'
                          AND h.old_status IN ('soft_suspended',
                                               'hard_suspended',
                                               'grace_period'))  AS reactivations,
       -- Suspension is deliberately NOT churn. A hard-suspended account is a
       -- collections problem and usually returns; counting it as churn makes
       -- every dunning run look like a customer exodus and leaves the two
       -- numbers impossible to act on separately.
       count(*) FILTER (WHERE h.new_status = 'terminated')       AS churned,
       count(*) FILTER (WHERE h.new_status IN ('soft_suspended',
                                               'hard_suspended')) AS suspended,
       count(*) FILTER (WHERE h.old_status IS NULL)
         - count(*) FILTER (WHERE h.new_status = 'terminated')   AS net_growth
  FROM subscriber_status_history h
 WHERE NOT h.is_baseline
 GROUP BY 1, 2, 3;

-- ── 3. Ticket resolution — FR-RPT-001 ────────────────────────────────────────
CREATE MATERIALIZED VIEW mv_ticket_resolution AS
WITH first_resolution AS (
    -- FIRST arrival at resolved, not last. A ticket closed, reopened and
    -- closed again is a support failure; taking the last timestamp would
    -- report it as one slow success and hide the reopen entirely.
    SELECT ticket_id, min(occurred_at) AS resolved_at
      FROM ticket_status_history
     WHERE new_status = 'resolved'
     GROUP BY ticket_id
),
reopens AS (
    SELECT ticket_id, count(*) AS reopen_count
      FROM ticket_status_history
     WHERE old_status = 'resolved' AND new_status <> 'closed'
     GROUP BY ticket_id
)
SELECT date_trunc('month', t.created_at)                    AS month,
       t.category,
       t.priority,
       t.franchise_id,
       count(*)                                             AS raised,
       count(fr.resolved_at)                                AS resolved,
       coalesce(sum(r.reopen_count), 0)                     AS reopens,
       -- Median, not mean: one ticket left open over a long weekend drags an
       -- average far enough to hide an otherwise healthy month.
       percentile_cont(0.5) WITHIN GROUP (
           ORDER BY EXTRACT(EPOCH FROM (fr.resolved_at - t.created_at)) / 3600.0
       )                                                    AS median_resolution_hours,
       count(*) FILTER (WHERE fr.resolved_at <= t.sla_resolution_due_at) AS resolved_within_sla
  -- LEFT JOIN so an unresolved ticket still counts in `raised`. An inner join
  -- would drop exactly the tickets a resolution report exists to surface, and
  -- the numbers would improve the worse things actually got.
  FROM tickets t
  LEFT JOIN first_resolution fr ON fr.ticket_id = t.id
  LEFT JOIN reopens r          ON r.ticket_id = t.id
 GROUP BY 1, 2, 3, 4;

-- REFRESH MATERIALIZED VIEW CONCURRENTLY requires a unique index. Without one
-- the refresh takes an ACCESS EXCLUSIVE lock and every dashboard reading the
-- view blocks behind it — the failure shows up as the reporting page hanging
-- during the refresh, which is exactly when somebody is looking at it.
--
-- The index must be over plain COLUMNS: Postgres rejects a concurrent refresh
-- backed by an expression index with "cannot refresh materialized view
-- concurrently". The obvious formulation here, coalesce(franchise_id, -1) to
-- cope with direct subscribers having no franchise, is exactly that and was
-- verified to fail.
--
-- NULLS NOT DISTINCT (PostgreSQL 15+) solves the same problem the coalesce was
-- reaching for: NULL franchise_id is a legitimate grouping — direct
-- subscribers — and without this clause those rows would not collide with each
-- other, leaving the index unable to identify a row for the refresh to match.
CREATE UNIQUE INDEX idx_mv_ticket_resolution_key
    ON mv_ticket_resolution (month, category, priority, franchise_id) NULLS NOT DISTINCT;

-- ── 4. Collection performance by franchise — FR-RPT-003 ──────────────────────
-- Billed and collected are counted from different tables on purpose: invoices
-- record what was charged, lco_ledger records what a franchise actually took
-- in. Deriving one from the other would make the collection rate definitionally
-- 100% and the report worthless.
CREATE OR REPLACE VIEW v_franchise_collection AS
WITH months AS (
    SELECT DISTINCT date_trunc('month', created_at) AS month FROM lco_ledger
    UNION
    SELECT DISTINCT date_trunc('month', created_at) FROM invoices
),
collected AS (
    SELECT franchise_id,
           date_trunc('month', created_at)   AS month,
           sum(recharge_amount)              AS collected,
           sum(commission_amount)            AS commission,
           count(DISTINCT subscriber_id)     AS paying_subscribers
      FROM lco_ledger
     GROUP BY 1, 2
),
billed AS (
    SELECT s.franchise_id,
           date_trunc('month', i.created_at) AS month,
           sum(i.total_amount)               AS billed,
           count(*)                          AS invoices_raised
      FROM invoices i
      JOIN subscribers s ON s.id = i.subscriber_id
     GROUP BY 1, 2
)
SELECT f.id                                  AS franchise_id,
       f.name                                AS franchise_name,
       f.status                              AS franchise_status,
       m.month,
       coalesce(b.billed, 0)::NUMERIC(14, 2)      AS billed,
       coalesce(b.invoices_raised, 0)             AS invoices_raised,
       coalesce(c.collected, 0)::NUMERIC(14, 2)   AS collected,
       coalesce(c.commission, 0)::NUMERIC(14, 2)  AS commission,
       coalesce(c.paying_subscribers, 0)          AS paying_subscribers,
       -- NULL rather than 0 when nothing was billed. A franchise that raised
       -- no invoices has no collection rate; reporting 0% would put a new
       -- territory at the bottom of a league table it has not yet joined.
       CASE WHEN coalesce(b.billed, 0) > 0
            THEN round(100.0 * coalesce(c.collected, 0) / b.billed, 2)
       END                                        AS collection_rate_pct
  FROM franchises f
  CROSS JOIN months m
  LEFT JOIN collected c ON c.franchise_id = f.id AND c.month = m.month
  LEFT JOIN billed    b ON b.franchise_id = f.id AND b.month = m.month;

-- ── 5. Privileges ────────────────────────────────────────────────────────────
-- The application connects as bss_app (migration 019), not as the role that
-- runs migrations, so the views need explicit grants for the same reason
-- staff_users did in 021: ALTER DEFAULT PRIVILEGES only covers objects created
-- by the role that set it.
--
-- Refresh needs more than a grant. PostgreSQL has no REFRESH privilege — the
-- command requires *ownership* of the materialised view — so bss_app either
-- owns it or cannot refresh it. Handing over ownership would also hand over
-- the right to drop it, which is a poor trade for a role deliberately built
-- with least privilege.
--
-- A SECURITY DEFINER function is the narrow alternative: it executes as its
-- owner (the migration role, which does own the view) and grants bss_app the
-- ability to refresh and nothing else. search_path is pinned because a
-- SECURITY DEFINER function with a caller-controlled search_path is a
-- privilege-escalation vector — an attacker who can create objects in an
-- earlier schema could shadow the ones the body resolves.
--
-- Found in live verification, not by the tests: the integration suite connects
-- as the superuser, so the refresh worked there and failed on the demo stack
-- with "must be owner of materialized view".
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION refresh_reporting_views() RETURNS void AS $$
BEGIN
    REFRESH MATERIALIZED VIEW CONCURRENTLY mv_ticket_resolution;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public;
-- +goose StatementEnd

REVOKE ALL ON FUNCTION refresh_reporting_views() FROM PUBLIC;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'bss_app') THEN
        GRANT SELECT ON v_plan_mix, v_subscriber_growth_monthly,
                        v_franchise_collection, mv_ticket_resolution TO bss_app;
        GRANT EXECUTE ON FUNCTION refresh_reporting_views() TO bss_app;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS refresh_reporting_views();
DROP VIEW IF EXISTS v_franchise_collection;
DROP INDEX IF EXISTS idx_mv_ticket_resolution_key;
DROP MATERIALIZED VIEW IF EXISTS mv_ticket_resolution;
DROP VIEW IF EXISTS v_subscriber_growth_monthly;
DROP VIEW IF EXISTS v_plan_mix;
