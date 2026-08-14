-- +goose Up
-- Session DB-028 | FR-NOTIF-012..013, FR-ANN-001..002 | DBD §6.2
-- announcements, subscriber_push_tokens | MDS §4.17
--
-- Completes the four-channel notification service MDS §4.7 described: email
-- has been loggable since migration 008 but never sendable, and push was not
-- represented at all.

-- ── 1. notification_log.channel: allow push ──────────────────────────────────
-- Migration 008 allowed whatsapp/sms/email. A push notification had nowhere
-- to be recorded, which would have made FR-NOTIF-009's "every outbound
-- notification creates a log record" impossible to honour for that channel.
ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_channel_check;
ALTER TABLE notification_log
    ADD CONSTRAINT notification_log_channel_check
        CHECK (channel IN ('whatsapp','sms','email','push'));

-- ── 2. subscriber_push_tokens (FR-NOTIF-013, FR-MOB-001) ─────────────────────
-- A table rather than a column on subscribers: one subscriber routinely has
-- several devices, and the same physical device re-registering must update
-- rather than accumulate duplicates — hence the unique token.
CREATE TABLE IF NOT EXISTS subscriber_push_tokens (
    id            SERIAL       PRIMARY KEY,
    subscriber_id INTEGER      NOT NULL REFERENCES subscribers(id) ON DELETE CASCADE,
    token         VARCHAR(255) NOT NULL UNIQUE,
    platform      VARCHAR(20)  NOT NULL CHECK (platform IN ('ios','android','web')),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_seen_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_push_tokens_subscriber ON subscriber_push_tokens(subscriber_id);

-- ── 3. announcements (FR-ANN-001..002) ───────────────────────────────────────
CREATE TABLE IF NOT EXISTS announcements (
    id                   SERIAL        PRIMARY KEY,
    title                VARCHAR(200)  NOT NULL,
    body                 TEXT          NOT NULL,
    -- Which dispatched channels to fan out to. The portal banner is
    -- show_in_portal below, not a channel: nothing is transmitted, so it has
    -- no delivery status and must not pollute notification_log.
    channels             TEXT[]        NOT NULL,
    -- Marketing by default, which is what gives dnd_opt_out meaning
    -- (FR-NOTIF-008). Marking an announcement transactional is a deliberate
    -- human decision the audit log records, not a quiet default that
    -- overrides everybody's opt-out.
    class                VARCHAR(20)   NOT NULL DEFAULT 'marketing'
                             CHECK (class IN ('marketing','transactional')),

    -- Segment: each NULL means "no filter on this dimension".
    segment_franchise_id INTEGER       REFERENCES franchises(id) ON DELETE SET NULL,
    segment_plan_id      INTEGER       REFERENCES plans(id) ON DELETE SET NULL,
    segment_status       VARCHAR(20),

    show_in_portal       BOOLEAN       NOT NULL DEFAULT FALSE,
    status               VARCHAR(20)   NOT NULL DEFAULT 'draft'
                             CHECK (status IN ('draft','sending','sent','failed')),
    recipient_count      INTEGER       NOT NULL DEFAULT 0,
    created_by_username  VARCHAR(100)  NOT NULL,
    created_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    sent_at              TIMESTAMPTZ,

    -- An announcement addressed to nothing at all is a drafting mistake, not
    -- a valid broadcast: it would report success having reached nobody.
    --
    -- cardinality(), not array_length(): array_length on an empty array
    -- returns NULL rather than 0, and a CHECK passes on NULL — so the
    -- array_length form silently admitted exactly the row it was written to
    -- reject. cardinality() returns 0.
    CONSTRAINT chk_announcement_has_destination CHECK (
        show_in_portal OR cardinality(channels) >= 1
    ),
    CONSTRAINT chk_announcement_channels CHECK (
        channels <@ ARRAY['whatsapp','sms','email','push']::TEXT[]
    )
);

-- The portal banner read: active announcements, newest first.
CREATE INDEX IF NOT EXISTS idx_announcements_portal ON announcements(created_at DESC)
    WHERE show_in_portal AND status = 'sent';
CREATE INDEX IF NOT EXISTS idx_announcements_status ON announcements(status, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_announcements_status;
DROP INDEX IF EXISTS idx_announcements_portal;
DROP TABLE IF EXISTS announcements CASCADE;

DROP INDEX IF EXISTS idx_push_tokens_subscriber;
DROP TABLE IF EXISTS subscriber_push_tokens CASCADE;

ALTER TABLE notification_log DROP CONSTRAINT IF EXISTS notification_log_channel_check;
ALTER TABLE notification_log
    ADD CONSTRAINT notification_log_channel_check
        CHECK (channel IN ('whatsapp','sms','email'));
