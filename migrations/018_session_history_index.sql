-- +goose Up
-- Session DB-018 | FR-SUB-001 | supports the subscriber portal's usage history page
--
-- The only existing per-subscriber index on subscriber_session_history
-- (idx_session_active_subscriber, migration 010) is a partial index limited
-- to WHERE stop_time IS NULL — it only helps find a subscriber's one
-- currently-active session. A usage history page needs a subscriber's past
-- (already-closed) sessions too, ordered by start_time, which that partial
-- index cannot serve; without this, that query sequential-scans every
-- partition of the table.
CREATE INDEX IF NOT EXISTS idx_session_history_subscriber
    ON subscriber_session_history(subscriber_id, start_time DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_session_history_subscriber;
