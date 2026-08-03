# Document 6: Database Design Document (DBD)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** DBD
**Traces From:** [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md)
**Traces To:** [API](07_API_OpenAPI_Contract.md) → [TST](13_TST_Test_Strategy.md)

---

## 6.1 Entity-Relationship Overview

```
plans (1) ─────────── (N) subscribers
subscribers (1) ─────── (N) invoices
subscribers (1) ─────── (N) wallet_ledgers
subscribers (1) ─────── (N) subscriber_session_history  [partitioned monthly]
subscribers (1) ─────── (N) kyc_verifications
subscribers (1) ─────── (N) cgnat_allocations           [partitioned monthly]
subscribers (1) ─────── (N) tickets
subscribers (1) ─────── (N) notification_log            [NEW — FR-NOTIF-009]
subscribers (1) ─────── (N) usage_snapshots             [NEW — FR-SUB-001]
franchises  (1) ─────── (N) subscribers                 [NEW — FR-FRN-001]
franchises  (1) ─────── (N) lco_ledger                  [NEW — FR-FRN-002]
encryption_keys (1) ─── (N) kyc_verifications
notification_templates  ─── (N) notification_log        [NEW — FR-NOTIF-010]
revenue_snapshots ─────────── (standalone — FR-REV-001) [NEW]
collections_forecast ──────── (standalone — FR-REV-004) [NEW]
lea_audit_log ─────────────── (append-only — FR-OBS-003)
```

---

## 6.2 Data Dictionary

### Table: `plans`
**FR:** FR-AAA-004, FR-FUP-001 | **Module:** MOD-AAA, MOD-FUP

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `name` | `VARCHAR(100)` | NOT NULL | |
| `rate_limit_string` | `VARCHAR(50)` | NOT NULL | MikroTik format: `100M/100M` |
| `volume_gb` | `INTEGER` | NOT NULL | Included data volume |
| `fup_threshold_bytes` | `BIGINT` | DEFAULT 0 | Pre-computed byte cap; 0 = unlimited |
| `fup_throttle_string` | `VARCHAR(50)` | NULLABLE | Post-FUP rate limit |
| `price` | `NUMERIC(12,2)` | NOT NULL | Base price excl. tax |
| `validity_days` | `INTEGER` | NOT NULL | |
| `franchise_id` | `INTEGER` | FK → franchises.id, NULLABLE | NULL = all franchises |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `subscribers`
**FR:** FR-AAA-001..004, FR-BIL-001, FR-FRN-001 | **Module:** MOD-AAA, MOD-BIL

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `caf_number` | `VARCHAR(50)` | UNIQUE NOT NULL | Official CAF registration index |
| `username` | `VARCHAR(100)` | UNIQUE NOT NULL | PPPoE/IPoE username |
| `password_hash` | `TEXT` | NOT NULL | bcrypt(cost=12); never plaintext |
| `mobile_number` | `VARCHAR(20)` | NOT NULL | E.164: `+91XXXXXXXXXX` |
| `email` | `VARCHAR(255)` | NULLABLE | |
| `plan_id` | `INTEGER` | FK → plans.id | |
| `franchise_id` | `INTEGER` | FK → franchises.id, NULLABLE | LCO owner; NULL = direct subscriber |
| `status` | `VARCHAR(20)` | NOT NULL | `active`, `grace_period`, `soft_suspended`, `hard_suspended`, `terminated` |
| `dunning_state` | `VARCHAR(20)` | NOT NULL DEFAULT 'active' | Maps to dunning state machine |
| `wallet_balance` | `NUMERIC(12,2)` | DEFAULT 0.00 | |
| `ipv4_address` | `INET` | NULLABLE | Static; NULL = dynamic |
| `registered_state` | `VARCHAR(10)` | NOT NULL | ISO state code for GST routing |
| `dnd_opt_out` | `BOOLEAN` | DEFAULT FALSE | TRAI DND flag |
| `kyc_status` | `VARCHAR(20)` | DEFAULT 'pending' | |
| `plan_expiry` | `TIMESTAMPTZ` | NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `franchises` *(new — FR-FRN-001)*
**Module:** MOD-FRN

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `name` | `VARCHAR(100)` | NOT NULL | LCO / franchise name |
| `owner_name` | `VARCHAR(100)` | NOT NULL | |
| `mobile_number` | `VARCHAR(20)` | NOT NULL | |
| `commission_rate_pct` | `NUMERIC(5,2)` | NOT NULL | Commission % per recharge |
| `status` | `VARCHAR(20)` | DEFAULT 'active' | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `lco_ledger` *(new — FR-FRN-002)*
**Module:** MOD-FRN

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `franchise_id` | `INTEGER` | FK → franchises.id NOT NULL | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | Subscriber whose recharge triggered this |
| `recharge_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `commission_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `transaction_ref` | `VARCHAR(100)` | | Links to wallet_ledgers.transaction_token |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `notification_log` *(new — FR-NOTIF-009)*
**Module:** MOD-NOTIF

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `BIGSERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id NOT NULL | |
| `channel` | `VARCHAR(20)` | NOT NULL | `whatsapp`, `sms`, `email` |
| `template_id` | `VARCHAR(20)` | FK → notification_templates.id NULLABLE | e.g. `TMPL-001` |
| `triggered_by_event` | `VARCHAR(50)` | NOT NULL | e.g. `fup_warning`, `dunning_remind_7d` |
| `triggered_by_entity_id` | `INTEGER` | NULLABLE | e.g. invoice ID, session ID |
| `sent_at` | `TIMESTAMPTZ` | NOT NULL DEFAULT NOW() | |
| `delivery_status` | `VARCHAR(20)` | NOT NULL DEFAULT 'sent' | `sent`, `delivered`, `read`, `failed`, `suppressed_dnd` |
| `failure_reason` | `TEXT` | NULLABLE | Provider error message |
| `provider_message_id` | `VARCHAR(100)` | NULLABLE | WhatsApp message ID for callback matching |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | Updated on delivery status callback |

### Table: `notification_templates` *(new — FR-NOTIF-010)*
**Module:** MOD-NOTIF

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `VARCHAR(20)` | PK | e.g. `TMPL-001` |
| `channel` | `VARCHAR(20)` | NOT NULL | `whatsapp`, `sms`, `email` |
| `template_name` | `VARCHAR(100)` | NOT NULL | Meta-approved template name |
| `event_trigger` | `VARCHAR(50)` | NOT NULL | e.g. `fup_warning` |
| `variables_schema` | `JSONB` | | Variable names and order |
| `active` | `BOOLEAN` | DEFAULT TRUE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `invoices`
**FR:** FR-BIL-001..002, FR-BIL-007 | **Module:** MOD-BIL

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `base_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `cgst_amount` | `NUMERIC(12,2)` | NOT NULL DEFAULT 0 | |
| `sgst_amount` | `NUMERIC(12,2)` | NOT NULL DEFAULT 0 | |
| `igst_amount` | `NUMERIC(12,2)` | NOT NULL DEFAULT 0 | |
| `total_amount` | `NUMERIC(12,2)` | NOT NULL | |
| `gst_rate_id` | `INTEGER` | FK → gst_rates.id | |
| `gb_included` | `INTEGER` | NOT NULL | Plan volume for usage summary on invoice |
| `gb_used` | `NUMERIC(10,2)` | NOT NULL | Actual usage for plain-language summary (FR-BIL-007) |
| `pdf_path` | `TEXT` | NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `CONSTRAINT chk_gst_logic` | | `(cgst_amount>0 AND igst_amount=0) OR (igst_amount>0 AND cgst_amount=0) OR (cgst_amount=0 AND igst_amount=0)` | |

### Table: `wallet_ledgers`
**FR:** FR-BIL-003, FR-BIL-005, FR-REV-002 | **Module:** MOD-BIL

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `franchise_id` | `INTEGER` | FK → franchises.id NULLABLE | For LCO tracking |
| `entry_type` | `VARCHAR(20)` | NOT NULL | `credit`, `debit` |
| `amount` | `NUMERIC(12,2)` | NOT NULL | |
| `balance_after` | `NUMERIC(12,2)` | NOT NULL | Running balance snapshot |
| `transaction_token` | `VARCHAR(100)` | UNIQUE NULLABLE | Idempotency key |
| `description` | `TEXT` | | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `subscriber_session_history`
**FR:** FR-NET-001..003 | Partitioned monthly on `start_time`

| Column | Type | Description |
|---|---|---|
| `id` | `BIGSERIAL` PK | |
| `subscriber_id` | `INTEGER` FK | |
| `session_id` | `VARCHAR(255)` | RADIUS Acct-Session-Id |
| `nas_ip_address` | `INET` | |
| `assigned_ipv4` | `INET` NULLABLE | |
| `assigned_ipv6_prefix` | `CIDR` NULLABLE | IPv6 PD |
| `start_time` | `TIMESTAMPTZ` | Partition key |
| `stop_time` | `TIMESTAMPTZ` NULLABLE | NULL = active |
| `input_octets` | `BIGINT` DEFAULT 0 | |
| `output_octets` | `BIGINT` DEFAULT 0 | |
| `terminate_cause` | `VARCHAR(50)` NULLABLE | |

### Table: `tickets`
**FR:** FR-SUB-004 | **Module:** MOD-PORTAL

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `category` | `VARCHAR(50)` | NOT NULL | `connectivity`, `billing`, `plan_change`, `other` |
| `description` | `TEXT` | NOT NULL | |
| `status` | `VARCHAR(20)` | DEFAULT 'open' | `open`, `in_progress`, `resolved`, `closed` |
| `assigned_to` | `INTEGER` | FK → admin_users.id NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `updated_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `revenue_snapshots` *(new — FR-REV-001)*
**Module:** MOD-REV

| Column | Type | Description |
|---|---|---|
| `id` | `SERIAL` PK | |
| `snapshot_date` | `DATE` NOT NULL | |
| `unbilled_subscriber_count` | `INTEGER` NOT NULL | |
| `ledger_variance` | `NUMERIC(12,2)` NOT NULL | Should be 0.00 |
| `total_wallet_balance` | `NUMERIC(14,2)` NOT NULL | |
| `created_at` | `TIMESTAMPTZ` DEFAULT NOW() | |

### Table: `collections_forecast` *(new — FR-REV-004)*
**Module:** MOD-REV

| Column | Type | Description |
|---|---|---|
| `id` | `SERIAL` PK | |
| `forecast_date` | `DATE` NOT NULL | Date forecast was generated |
| `forecast_for_date` | `DATE` NOT NULL | Future date being forecast |
| `expected_renewals` | `INTEGER` | Subscribers with wallet ≥ plan price |
| `at_risk_renewals` | `INTEGER` | Subscribers with wallet < plan price |
| `expected_revenue` | `NUMERIC(14,2)` | |
| `at_risk_revenue` | `NUMERIC(14,2)` | |

### Table: `kyc_verifications`
**FR:** FR-SEC-002..003 | **Module:** MOD-AUTH

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `subscriber_id` | `INTEGER` | FK → subscribers.id | |
| `aadhaar_encrypted` | `TEXT` | NULLABLE | `{key_version}:{base64_ciphertext}` |
| `pan_encrypted` | `TEXT` | NULLABLE | `{key_version}:{base64_ciphertext}` |
| `key_version_id` | `VARCHAR(10)` | FK → encryption_keys.version_id | |
| `verified_at` | `TIMESTAMPTZ` | NULLABLE | |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |

### Table: `encryption_keys`
**FR:** FR-SEC-002..003 | **Module:** MOD-AUTH

| Column | Type | Constraints | Description |
|---|---|---|---|
| `id` | `SERIAL` | PK | |
| `version_id` | `VARCHAR(10)` | UNIQUE NOT NULL | e.g. `v1`, `v2`, `v3` |
| `key_hash` | `VARCHAR(64)` | NOT NULL | SHA-256 of key material for audit |
| `created_at` | `TIMESTAMPTZ` | DEFAULT NOW() | |
| `rotated_at` | `TIMESTAMPTZ` | NULLABLE | |
| `status` | `VARCHAR(10)` | DEFAULT 'active' | `active` or `retired` |

### Table: `cgnat_allocations`
**FR:** FR-NET-001..002 | Partitioned monthly on `allocated_at`

| Column | Type | Description |
|---|---|---|
| `id` | `BIGSERIAL` PK | |
| `subscriber_id` | `INTEGER` FK | |
| `public_ip` | `INET` NOT NULL | |
| `port_start` / `port_end` | `INTEGER` NOT NULL | |
| `nas_ip_address` | `INET` | |
| `allocated_at` | `TIMESTAMPTZ` | Partition key |
| `released_at` | `TIMESTAMPTZ` NULLABLE | |

### Table: `lea_audit_log`
**FR:** FR-OBS-003 | Append-only; row security policy (INSERT only)

| Column | Type | Description |
|---|---|---|
| `id` | `BIGSERIAL` PK | |
| `accessor_identity` | `VARCHAR(255)` NOT NULL | JWT `sub` claim |
| `accessor_role` | `VARCHAR(50)` NOT NULL | |
| `queried_public_ip` | `INET` NOT NULL | |
| `queried_port` | `INTEGER` NULLABLE | |
| `queried_timestamp` | `TIMESTAMPTZ` NOT NULL | |
| `result_subscriber_id` | `INTEGER` NULLABLE | |
| `result_row_count` | `INTEGER` NOT NULL | |
| `accessed_at` | `TIMESTAMPTZ` DEFAULT NOW() | |

---

## 6.3 Partitioning Strategy

```sql
SELECT partman.create_parent('public.subscriber_session_history', 'start_time', 'monthly', 3);
SELECT partman.create_parent('public.cgnat_allocations', 'allocated_at', 'monthly', 3);
```

---

## 6.4 Index Definitions

```sql
-- LEA: IP-to-subscriber lookup
CREATE INDEX idx_lea_ipv4_time ON subscriber_session_history(assigned_ipv4, start_time DESC)
  INCLUDE (subscriber_id, stop_time);

-- LEA: CGNAT port-block lookup
CREATE INDEX idx_cgnat_lea ON cgnat_allocations(public_ip, allocated_at DESC)
  INCLUDE (subscriber_id, port_start, port_end, released_at);

-- AAA: fast subscriber auth
CREATE INDEX idx_sub_auth ON subscribers(username, status);

-- AAA: active session cleanup on NAS reconnect
CREATE INDEX idx_nas_active ON subscriber_session_history(nas_ip_address) WHERE stop_time IS NULL;

-- Billing: dunning expiry scan
CREATE INDEX idx_sub_expiry ON subscribers(plan_expiry, status) WHERE status IN ('active','grace_period');

-- Billing: wallet idempotency
CREATE UNIQUE INDEX idx_wallet_token ON wallet_ledgers(transaction_token) WHERE transaction_token IS NOT NULL;

-- Notifications: subscriber notification history (FR-SUB-005)
CREATE INDEX idx_notif_subscriber ON notification_log(subscriber_id, sent_at DESC);

-- Notifications: delivery status callback lookup by provider_message_id (FR-NOTIF-011)
CREATE INDEX idx_notif_provider_id ON notification_log(provider_message_id) WHERE provider_message_id IS NOT NULL;

-- Revenue: unbilled subscriber report (FR-REV-001)
CREATE INDEX idx_revenue_unbilled ON subscribers(status, plan_expiry) WHERE status = 'active';

-- Franchise: LCO subscriber isolation
CREATE INDEX idx_franchise_subscribers ON subscribers(franchise_id) WHERE franchise_id IS NOT NULL;
```

---

## 6.5 Row Security — LEA Audit Log

```sql
ALTER TABLE lea_audit_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY lea_insert_only ON lea_audit_log FOR INSERT WITH CHECK (true);
```
