-- +goose Up
-- Session DB-015 (part 1) | DBD §6.4 — remaining indexes deferred to here
-- franchise_id index on subscribers (referenced in DB-012 and DBD §6.4)

CREATE INDEX IF NOT EXISTS idx_franchise_subscribers ON subscribers(franchise_id)
    WHERE franchise_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_franchise_subscribers;
