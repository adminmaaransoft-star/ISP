-- +goose Up
-- Session DB-008 | FR-NOTIF-009..011 | DBD §6.2 notification_templates + notification_log

CREATE TABLE IF NOT EXISTS notification_templates (
    id               VARCHAR(20)     PRIMARY KEY,    -- e.g. TMPL-001
    channel          VARCHAR(20)     NOT NULL CHECK (channel IN ('whatsapp','sms','email')),
    template_name    VARCHAR(100)    NOT NULL,        -- Meta-approved template name
    event_trigger    VARCHAR(50)     NOT NULL,        -- e.g. fup_warning
    variables_schema JSONB,                           -- variable names and order
    active           BOOLEAN         NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS notification_log (
    id                       BIGSERIAL       PRIMARY KEY,
    subscriber_id            INTEGER         NOT NULL REFERENCES subscribers(id),
    channel                  VARCHAR(20)     NOT NULL CHECK (channel IN ('whatsapp','sms','email')),
    template_id              VARCHAR(20)     REFERENCES notification_templates(id),  -- NULLABLE: system events may have none
    triggered_by_event       VARCHAR(50)     NOT NULL,
    triggered_by_entity_id   INTEGER,
    sent_at                  TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    delivery_status          VARCHAR(20)     NOT NULL DEFAULT 'sent'
                                 CHECK (delivery_status IN ('sent','delivered','read','failed','suppressed_dnd')),
    failure_reason           TEXT,
    provider_message_id      VARCHAR(100),            -- WhatsApp message ID for callback matching
    updated_at               TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

-- DBD §6.4 indexes
CREATE INDEX idx_notif_subscriber ON notification_log(subscriber_id, sent_at DESC);

-- Partial index for delivery callback lookup by provider_message_id
CREATE INDEX idx_notif_provider_id ON notification_log(provider_message_id)
    WHERE provider_message_id IS NOT NULL;

-- updated_at trigger (reuse function from migration 003)
CREATE TRIGGER trg_notification_log_updated_at
    BEFORE UPDATE ON notification_log
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS trg_notification_log_updated_at ON notification_log;
DROP TABLE IF EXISTS notification_log CASCADE;
DROP TABLE IF EXISTS notification_templates CASCADE;
