-- +goose Up
-- Session DB-009 | FR-SUB-004 | DBD §6.2 tickets

CREATE TABLE IF NOT EXISTS tickets (
    id            SERIAL       PRIMARY KEY,
    subscriber_id INTEGER      NOT NULL REFERENCES subscribers(id),
    category      VARCHAR(50)  NOT NULL
                      CHECK (category IN ('connectivity','billing','plan_change','other')),
    description   TEXT         NOT NULL,
    status        VARCHAR(20)  NOT NULL DEFAULT 'open'
                      CHECK (status IN ('open','in_progress','resolved','closed')),
    assigned_to   INTEGER,     -- FK to admin_users.id added in future migration
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- updated_at trigger
CREATE TRIGGER trg_tickets_updated_at
    BEFORE UPDATE ON tickets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS trg_tickets_updated_at ON tickets;
DROP TABLE IF EXISTS tickets CASCADE;
