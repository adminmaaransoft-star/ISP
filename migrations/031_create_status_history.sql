-- +goose Up
-- Session DB-031 | FR-RPT-001 | DBD §6.2 subscriber_status_history,
-- ticket_status_history | MDS §4.8 (extended)
--
-- Reporting cannot read history the database never wrote down.
--
-- subscribers.status and tickets.status are both overwritten in place, and
-- updated_at is bumped by every unrelated edit, so nothing currently records
-- *when* an account churned or *when* a ticket was resolved. A view can
-- aggregate history but cannot invent it: every day without these tables is
-- churn and resolution data destroyed as it is produced. That is why this
-- migration ships on its own, ahead of any reporting view that reads it.
--
-- Capture is by trigger rather than by application code. There are only four
-- status-writing paths today, but a trigger holds for the fifth one somebody
-- adds next year, and for a DBA fixing a row by hand at 2am — the cases where
-- a missing audit row is least likely to be noticed and most likely to matter.
-- Attribution (who, why) still comes from the application, through a
-- transaction-local setting the trigger reads. If the caller forgets to set
-- it the transition is still recorded, with changed_by 'unknown': losing who
-- made a change is recoverable, losing the fact that it happened is not.

-- ── 1. Subscriber status transitions ─────────────────────────────────────────
CREATE TABLE IF NOT EXISTS subscriber_status_history (
    id             BIGSERIAL PRIMARY KEY,
    subscriber_id  INTEGER      NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    -- NULL means "no previous status": either the account was just created,
    -- or this is the baseline snapshot seeded below.
    old_status     VARCHAR(20),
    new_status     VARCHAR(20)  NOT NULL,
    reason         VARCHAR(64),
    changed_by     VARCHAR(100) NOT NULL DEFAULT 'unknown',

    -- Denormalised on purpose. Churn analysis asks "which plan did they leave,
    -- in whose territory" — joining to subscribers at report time answers
    -- "which plan do they hold now", which for a terminated account is the
    -- wrong question and for one that has since migrated is the wrong answer.
    plan_id        INTEGER REFERENCES plans(id),
    franchise_id   INTEGER REFERENCES franchises(id),

    -- TRUE only for the one-off snapshot this migration seeds for accounts
    -- that already existed. A snapshot is a starting position, not an event,
    -- and the reporting views must not count it as growth or churn — doing so
    -- would invent a signup curve nobody actually observed.
    is_baseline    BOOLEAN      NOT NULL DEFAULT FALSE,

    occurred_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    -- A save that does not change the status is not a transition. Without
    -- this, an operator re-saving an unchanged form inflates every count.
    CONSTRAINT chk_ssh_status_changed CHECK (old_status IS DISTINCT FROM new_status),
    CONSTRAINT chk_ssh_baseline_has_no_predecessor
        CHECK (NOT is_baseline OR old_status IS NULL)
);

CREATE INDEX IF NOT EXISTS idx_ssh_occurred ON subscriber_status_history (occurred_at DESC);
CREATE INDEX IF NOT EXISTS idx_ssh_subscriber ON subscriber_status_history (subscriber_id, occurred_at DESC);

-- ── 2. Ticket status transitions ─────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ticket_status_history (
    id           BIGSERIAL PRIMARY KEY,
    ticket_id    INTEGER      NOT NULL REFERENCES tickets(id) ON DELETE CASCADE,
    old_status   VARCHAR(20),
    new_status   VARCHAR(20)  NOT NULL,
    changed_by   VARCHAR(100) NOT NULL DEFAULT 'unknown',
    is_baseline  BOOLEAN      NOT NULL DEFAULT FALSE,
    occurred_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_tsh_status_changed CHECK (old_status IS DISTINCT FROM new_status),
    CONSTRAINT chk_tsh_baseline_has_no_predecessor
        CHECK (NOT is_baseline OR old_status IS NULL)
);

-- Every resolution metric starts by finding the FIRST transition into
-- 'resolved' for a ticket, so that is the access pattern indexed.
CREATE INDEX IF NOT EXISTS idx_tsh_ticket ON ticket_status_history (ticket_id, occurred_at);

-- ── 3. Capture triggers ──────────────────────────────────────────────────────
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_subscriber_status() RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO subscriber_status_history (
        subscriber_id, old_status, new_status, reason, changed_by,
        plan_id, franchise_id)
    VALUES (
        NEW.id,
        -- On INSERT there is no OLD record at all; TG_OP distinguishes the
        -- two cases so a new connection is recorded as a real event rather
        -- than being confused with the seeded baseline.
        CASE WHEN TG_OP = 'UPDATE' THEN OLD.status ELSE NULL END,
        NEW.status,
        nullif(current_setting('app.change_reason', true), ''),
        coalesce(nullif(current_setting('app.actor', true), ''), 'unknown'),
        NEW.plan_id, NEW.franchise_id);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capture_ticket_status() RETURNS TRIGGER AS $$
BEGIN
    INSERT INTO ticket_status_history (ticket_id, old_status, new_status, changed_by)
    VALUES (
        NEW.id,
        CASE WHEN TG_OP = 'UPDATE' THEN OLD.status ELSE NULL END,
        NEW.status,
        coalesce(nullif(current_setting('app.actor', true), ''), 'unknown'));
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS trg_subscriber_created ON subscribers;
CREATE TRIGGER trg_subscriber_created
    AFTER INSERT ON subscribers
    FOR EACH ROW EXECUTE FUNCTION capture_subscriber_status();

-- UPDATE OF status plus the WHEN clause means the function is not called at
-- all for the far more frequent wallet_balance, plan_expiry and fup_active
-- writes — the hot paths pay nothing for this.
DROP TRIGGER IF EXISTS trg_subscriber_status_changed ON subscribers;
CREATE TRIGGER trg_subscriber_status_changed
    AFTER UPDATE OF status ON subscribers
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION capture_subscriber_status();

DROP TRIGGER IF EXISTS trg_ticket_created ON tickets;
CREATE TRIGGER trg_ticket_created
    AFTER INSERT ON tickets
    FOR EACH ROW EXECUTE FUNCTION capture_ticket_status();

DROP TRIGGER IF EXISTS trg_ticket_status_changed ON tickets;
CREATE TRIGGER trg_ticket_status_changed
    AFTER UPDATE OF status ON tickets
    FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION capture_ticket_status();

-- ── 4. Baseline snapshot for rows that already exist ─────────────────────────
-- One row per live account, recording where it stands now — not a fabricated
-- transition history. occurred_at is the account's created_at so the snapshot
-- sorts before any real transition that follows it, and is_baseline keeps it
-- out of every growth and churn count.
--
-- Terminated accounts are skipped: they can produce no further transitions,
-- and a baseline row for one would be a churn we cannot date claiming to be a
-- signup we cannot date.
INSERT INTO subscriber_status_history (
    subscriber_id, old_status, new_status, reason, changed_by,
    plan_id, franchise_id, is_baseline, occurred_at)
SELECT s.id, NULL, s.status, 'baseline', 'system:migration-031',
       s.plan_id, s.franchise_id, TRUE, s.created_at
  FROM subscribers s
 WHERE s.status <> 'terminated'
   AND NOT EXISTS (SELECT 1 FROM subscriber_status_history h WHERE h.subscriber_id = s.id);

-- Resolved and closed tickets are skipped for the same reason: their
-- resolution moment is unrecoverable, and a baseline row would let them be
-- counted as raised-but-never-resolved and drag every resolution rate down.
INSERT INTO ticket_status_history (ticket_id, old_status, new_status, changed_by, is_baseline, occurred_at)
SELECT t.id, NULL, t.status, 'system:migration-031', TRUE, t.created_at
  FROM tickets t
 WHERE t.status NOT IN ('resolved', 'closed')
   AND NOT EXISTS (SELECT 1 FROM ticket_status_history h WHERE h.ticket_id = t.id);

-- +goose Down
DROP TRIGGER IF EXISTS trg_ticket_status_changed ON tickets;
DROP TRIGGER IF EXISTS trg_ticket_created ON tickets;
DROP TRIGGER IF EXISTS trg_subscriber_status_changed ON subscribers;
DROP TRIGGER IF EXISTS trg_subscriber_created ON subscribers;
DROP FUNCTION IF EXISTS capture_ticket_status();
DROP FUNCTION IF EXISTS capture_subscriber_status();
DROP INDEX IF EXISTS idx_tsh_ticket;
DROP TABLE IF EXISTS ticket_status_history CASCADE;
DROP INDEX IF EXISTS idx_ssh_subscriber;
DROP INDEX IF EXISTS idx_ssh_occurred;
DROP TABLE IF EXISTS subscriber_status_history CASCADE;
