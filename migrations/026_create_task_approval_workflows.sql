-- +goose Up
-- Session DB-026 | FR-WFL-001..002 | DBD §6.2 approval_requests, field_tasks
-- MDS §4.15 — second-approver sign-off for sensitive account actions, and
-- ad hoc field-task assignment independent of the ticket system.

-- ── approval_requests ────────────────────────────────────────────────────────
-- Staff wallet credit, refund and termination move from "execute
-- immediately" to "create a pending request, execute only once a different
-- staff member approves it" (MDS §4.15). status='approved' is a real,
-- persisted state — the atomic claim a decision makes before the
-- underlying action executes — not a transient value the app skips through,
-- so a crash between claim and execution leaves an honest, inspectable row
-- rather than one that looks untouched or one that risks re-executing.
CREATE TABLE IF NOT EXISTS approval_requests (
    id                     SERIAL          PRIMARY KEY,
    action_type            VARCHAR(30)     NOT NULL
                                CHECK (action_type IN ('wallet_credit','refund','terminate')),
    subscriber_id          INTEGER         NOT NULL REFERENCES subscribers(id),
    amount                 NUMERIC(12,2),                 -- NULL for terminate
    reason                 TEXT            NOT NULL,
    status                 VARCHAR(20)     NOT NULL DEFAULT 'pending'
                                CHECK (status IN ('pending','approved','rejected','executed','execution_failed')),
    requested_by_username  VARCHAR(100)    NOT NULL,
    decided_by_username    VARCHAR(100),
    decision_reason        TEXT,
    execution_error        TEXT,                          -- set only when status = execution_failed
    ledger_entry_id        INTEGER         REFERENCES wallet_ledgers(id),
    created_at             TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    decided_at             TIMESTAMPTZ,

    -- The second-approver guarantee at the schema level, not just app code:
    -- a request decided by the person who filed it is invalid regardless of
    -- which code path got there.
    CONSTRAINT chk_approval_distinct_approver
        CHECK (decided_by_username IS NULL OR decided_by_username <> requested_by_username),
    -- amount is required for the two money-moving action types and
    -- forbidden for terminate, so a malformed row can never silently mean
    -- "terminate for ₹0" or "credit with no amount."
    CONSTRAINT chk_approval_amount_by_type CHECK (
        (action_type = 'terminate' AND amount IS NULL) OR
        (action_type <> 'terminate' AND amount IS NOT NULL AND amount > 0)
    )
);

-- The approval queue's primary access pattern: "what's waiting on me."
CREATE INDEX IF NOT EXISTS idx_approvals_pending ON approval_requests(status, created_at)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_approvals_subscriber ON approval_requests(subscriber_id);

-- ── field_tasks ──────────────────────────────────────────────────────────────
-- Deliberately independent of tickets (CRD-EXP-002's own wording): internal
-- staff coordination, not subscriber-facing support, with no SLA engine or
-- routing rules of its own.
CREATE TABLE IF NOT EXISTS field_tasks (
    id                    SERIAL          PRIMARY KEY,
    title                 VARCHAR(200)    NOT NULL,
    description           TEXT,
    subscriber_id         INTEGER         REFERENCES subscribers(id),
    franchise_id          INTEGER         REFERENCES franchises(id),
    assigned_to_username  VARCHAR(100)    NOT NULL,
    created_by_username   VARCHAR(100)    NOT NULL,
    status                VARCHAR(20)     NOT NULL DEFAULT 'open'
                              CHECK (status IN ('open','in_progress','completed','cancelled')),
    due_date              DATE,
    created_at            TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    completed_at          TIMESTAMPTZ
);

CREATE TRIGGER trg_field_tasks_updated_at
    BEFORE UPDATE ON field_tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE INDEX IF NOT EXISTS idx_field_tasks_assignee ON field_tasks(assigned_to_username, status);

-- +goose Down
DROP TRIGGER IF EXISTS trg_field_tasks_updated_at ON field_tasks;
DROP TABLE IF EXISTS field_tasks CASCADE;

DROP INDEX IF EXISTS idx_approvals_subscriber;
DROP INDEX IF EXISTS idx_approvals_pending;
DROP TABLE IF EXISTS approval_requests CASCADE;
