-- +goose Up
-- Session DB-035 | FR-HSP-001 | DBD §6.9 | MDS §4.23
--
-- Usage accounting for voucher-backed hotspot grants.
--
-- Migration 034 gave vouchers a data_cap_bytes column and nothing ever read
-- it: a voucher sold as "1 GB" was limited only by its duration. The reason is
-- structural rather than an oversight. The FUP scanner finds over-quota
-- sessions by joining subscribers, and chk_grant_has_exactly_one_source makes
-- a voucher grant's subscriber_id NULL by design — so voucher sessions are
-- invisible to the machinery that throttles everyone else, and
-- subscriber_session_history cannot hold them either (its subscriber_id is NOT
-- NULL and carries a foreign key).
--
-- Rather than weaken that table's constraints for the minority case, usage for
-- these sessions is counted on the grant itself. The grant is already the
-- object that authorises the session and already expires; making it also the
-- thing that meters the session keeps the whole voucher lifecycle in one row.

ALTER TABLE hotspot_grants
    ADD COLUMN IF NOT EXISTS bytes_used     BIGINT      NOT NULL DEFAULT 0
        CHECK (bytes_used >= 0),
    -- The live RADIUS session, captured from accounting. Without it a quota
    -- breach has nothing to disconnect: a Disconnect-Request is addressed by
    -- Acct-Session-Id, not by MAC.
    ADD COLUMN IF NOT EXISTS session_id     VARCHAR(255),
    -- Where to send that Disconnect-Request. Taken from the NAS-IP-Address the
    -- device reports rather than the packet source, which behind NAT is
    -- somewhere else entirely.
    ADD COLUMN IF NOT EXISTS nas_ip_address INET,
    ADD COLUMN IF NOT EXISTS last_seen_at   TIMESTAMPTZ,
    -- Set when the cap is what ended the grant, so "your data ran out" can be
    -- told apart from "your time ran out" — the two need different answers at
    -- a counter, and revoked_at alone cannot distinguish them.
    ADD COLUMN IF NOT EXISTS exhausted_at   TIMESTAMPTZ;

COMMENT ON COLUMN hotspot_grants.bytes_used IS
    'FR-HSP-001: octets counted from RADIUS accounting for this grant. Voucher '
    'sessions have no subscriber row, so they are metered here rather than in '
    'subscriber_session_history.';

-- The quota scan: live grants, newest usage first. Partial on revoked_at so
-- the index stays the size of the currently-online population rather than the
-- history of every voucher ever sold.
CREATE INDEX IF NOT EXISTS idx_hotspot_grants_usage
    ON hotspot_grants (mac_address) WHERE revoked_at IS NULL;

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'bss_app') THEN
        GRANT SELECT, INSERT, UPDATE ON hotspot_grants TO bss_app;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP INDEX IF EXISTS idx_hotspot_grants_usage;
ALTER TABLE hotspot_grants
    DROP COLUMN IF EXISTS exhausted_at,
    DROP COLUMN IF EXISTS last_seen_at,
    DROP COLUMN IF EXISTS nas_ip_address,
    DROP COLUMN IF EXISTS session_id,
    DROP COLUMN IF EXISTS bytes_used;
