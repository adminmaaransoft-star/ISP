-- scripts/seed_local.sql
-- Local development seed data — DXD §11.4
-- Run after all migrations: psql -h localhost -U postgres -d isp_bss_oss -f scripts/seed_local.sql
-- Insertion order respects FK dependencies: franchises → plans → subscribers → rest

-- ── GST Rates ────────────────────────────────────────────────────────────────
INSERT INTO gst_rates (cgst_rate, sgst_rate, igst_rate, effective_from)
VALUES (9.00, 9.00, 18.00, NOW())
ON CONFLICT DO NOTHING;

-- ── Plans ────────────────────────────────────────────────────────────────────
INSERT INTO plans (name, rate_limit_string, volume_gb, fup_threshold_bytes, fup_throttle_string, price, validity_days)
VALUES
    ('TN_Basic_50M',  '50M/50M',   1650, 1771674009600, '5M/5M',   499.00, 30),
    ('TN_Super_100M', '100M/100M', 3300, 3543348019200, '10M/10M', 799.00, 30),
    ('TN_Ultra_200M', '200M/200M', 5000, 5368709120000, '20M/20M', 1199.00, 30)
ON CONFLICT DO NOTHING;

-- ── Notification Templates ───────────────────────────────────────────────────
INSERT INTO notification_templates (id, channel, template_name, event_trigger, active)
VALUES
    ('TMPL-001', 'whatsapp', 'fup_warning',          'fup_warning',       TRUE),
    ('TMPL-002', 'whatsapp', 'fup_throttle_applied',  'fup_throttle',      TRUE),
    ('TMPL-003', 'whatsapp', 'renewal_reminder',      'renewal_reminder',  TRUE),
    ('TMPL-004', 'whatsapp', 'soft_suspension',       'soft_suspension',   TRUE),
    ('TMPL-005', 'whatsapp', 'hard_suspension',       'hard_suspension',   TRUE),
    ('TMPL-006', 'whatsapp', 'service_restored',      'service_restored',  TRUE),
    ('TMPL-007', 'whatsapp', 'payment_received',      'payment_received',  TRUE),
    ('TMPL-008', 'whatsapp', 'ticket_update',         'ticket_update',     TRUE)
ON CONFLICT (id) DO NOTHING;

-- ── Subscribers ──────────────────────────────────────────────────────────────
-- password_hash values are bcrypt(cost=12) of "testpassword" — NEVER use in production
INSERT INTO subscribers
    (caf_number, username, password_hash, mobile_number, plan_id, status, wallet_balance, registered_state, dunning_state)
VALUES
    ('CAF-0001', 'test_user',
     '$2a$12$gn685/ch8wxn9XO33DI4lei.c/vdxKTXTg1VV0Cty2qHQLnnfnHbq',
     '+919876543210',
     (SELECT id FROM plans WHERE name = 'TN_Super_100M'),
     'active', 799.00, 'TN', 'active'),

    ('CAF-0002', 'suspended_user',
     '$2a$12$gn685/ch8wxn9XO33DI4lei.c/vdxKTXTg1VV0Cty2qHQLnnfnHbq',
     '+919876543211',
     (SELECT id FROM plans WHERE name = 'TN_Basic_50M'),
     'hard_suspended', 0.00, 'TN', 'active')
ON CONFLICT (username) DO NOTHING;

-- ── Wallet Ledger (initial credit for test_user) ─────────────────────────────
INSERT INTO wallet_ledgers (subscriber_id, entry_type, amount, balance_after, transaction_token, description)
SELECT id, 'credit', 799.00, 799.00, 'seed_tok_001', 'Initial seed credit'
FROM subscribers WHERE username = 'test_user'
ON CONFLICT DO NOTHING;

-- ── One Test Invoice ─────────────────────────────────────────────────────────
INSERT INTO invoices (subscriber_id, base_amount, cgst_amount, sgst_amount, igst_amount, total_amount, gst_rate_id, gb_included, gb_used)
SELECT
    s.id,
    799.00,
    71.91,
    71.91,
    0.00,
    942.82,
    g.id,
    3300,
    0.00
FROM subscribers s, gst_rates g
WHERE s.username = 'test_user'
ORDER BY g.id LIMIT 1
ON CONFLICT DO NOTHING;

-- ── Verify seed counts ────────────────────────────────────────────────────────
SELECT 'plans'                  AS table_name, COUNT(*) AS row_count FROM plans
UNION ALL
SELECT 'gst_rates',              COUNT(*) FROM gst_rates
UNION ALL
SELECT 'subscribers',            COUNT(*) FROM subscribers
UNION ALL
SELECT 'notification_templates', COUNT(*) FROM notification_templates
UNION ALL
SELECT 'wallet_ledgers',         COUNT(*) FROM wallet_ledgers
UNION ALL
SELECT 'invoices',               COUNT(*) FROM invoices;
