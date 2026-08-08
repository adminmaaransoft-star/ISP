-- +goose Up
-- Session DB-017 | FR-NET-002 | supports admin session control
--
-- The admin disconnect and FUP-override endpoints resolve a NAS-issued
-- session_id to the subscriber and NAS address that a PoD or CoA packet needs
-- to target. Without this index that lookup sequential-scans the current
-- month's partition of subscriber_session_history on every call.
CREATE INDEX IF NOT EXISTS idx_session_lookup ON subscriber_session_history(session_id)
    WHERE stop_time IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_session_lookup;
