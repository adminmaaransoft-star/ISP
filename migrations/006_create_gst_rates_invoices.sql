-- +goose Up
-- Session DB-006 | FR-BIL-001, FR-BIL-007 | DBD §6.2 gst_rates + invoices

CREATE TABLE IF NOT EXISTS gst_rates (
    id             SERIAL          PRIMARY KEY,
    cgst_rate      NUMERIC(5,2)    NOT NULL,
    sgst_rate      NUMERIC(5,2)    NOT NULL,
    igst_rate      NUMERIC(5,2)    NOT NULL,
    effective_from TIMESTAMPTZ     NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS invoices (
    id            SERIAL          PRIMARY KEY,
    subscriber_id INTEGER         NOT NULL REFERENCES subscribers(id),
    base_amount   NUMERIC(12,2)   NOT NULL,
    cgst_amount   NUMERIC(12,2)   NOT NULL DEFAULT 0.00,
    sgst_amount   NUMERIC(12,2)   NOT NULL DEFAULT 0.00,
    igst_amount   NUMERIC(12,2)   NOT NULL DEFAULT 0.00,
    total_amount  NUMERIC(12,2)   NOT NULL,
    gst_rate_id   INTEGER         NOT NULL REFERENCES gst_rates(id),
    gb_included   INTEGER         NOT NULL,           -- plan volume for invoice summary
    gb_used       NUMERIC(10,2)   NOT NULL,           -- actual usage
    pdf_path      TEXT,
    created_at    TIMESTAMPTZ     NOT NULL DEFAULT NOW(),

    -- GST logic: cannot have both intrastate (cgst/sgst) and interstate (igst) non-zero
    CONSTRAINT chk_gst_logic CHECK (
        (cgst_amount > 0 AND igst_amount = 0) OR
        (igst_amount > 0 AND cgst_amount = 0) OR
        (cgst_amount = 0 AND igst_amount = 0)
    )
);

-- +goose Down
DROP TABLE IF EXISTS invoices CASCADE;
DROP TABLE IF EXISTS gst_rates CASCADE;
