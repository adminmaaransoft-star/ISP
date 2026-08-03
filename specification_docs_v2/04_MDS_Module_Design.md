# Document 4: Module Design Specification (MDS)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** MDS
**Traces From:** [SAD](03_SAD_System_Architecture.md) → [SRS](02_SRS_System_Requirements.md)
**Traces To:** [DDS](05_DDS_Detailed_Design.md) → [DBD](06_DBD_Database_Design.md) → [API](07_API_OpenAPI_Contract.md) → [TST](13_TST_Test_Strategy.md)

---

## 4.1 Module 1: AAA Core Network Daemon
**Module ID:** MOD-AAA | **SAD Ref:** SAD-COMP-001 | **FR:** FR-AAA-001..004

`internal/radius` — Processes UDP streams on ports 1812/1813. Fixed 128-worker channel pool; never touches PostgreSQL on hot path.

**Responsibilities:** Receive and parse RADIUS packets, authenticate from Redis (PostgreSQL fallback on miss), write-through new sessions, deduplicate Interim-Update packets via SetNX, increment per-session traffic counters atomically, publish accounting events to Redis stream, enforce brute-force rate limiting via token bucket.

**Key Metrics Emitted:**
- `radius_auth_duration_seconds` (histogram, p50/p95/p99)
- `radius_auth_total` (counter, labels: `result=accept|reject|error`)
- `radius_accounting_packets_total` (counter, labels: `type=start|interim|stop`)
- `radius_dedup_skipped_total` (counter)
- `radius_worker_queue_depth` (gauge)
- `radius_nas_failure_rate` (gauge per NAS IP — feeds FR-OBS-005 alert)

---

## 4.2 Module 2: Cache & Real-Time FUP Monitoring
**Module ID:** MOD-FUP | **SAD Ref:** SAD-COMP-002 | **FR:** FR-FUP-001..005

`internal/fup` — Maintains write-through subscriber profiles in Redis. Background goroutine samples active sessions every 10s, compares usage vs threshold, emits Asynq tasks.

**FUP Threshold Events:**

| Threshold | Asynq Task Enqueued | Idempotency Key |
|---|---|---|
| 80% of plan bytes | `notif:fup_warning` | `fup_warn:{session_id}:{day}` |
| 100% of plan bytes | `coa:send` + `notif:fup_throttle` | `coa:{session_id}:{breach_epoch_minute}` |
| Plan expiry | `pod:send` | `pod:{session_id}:{expiry_date}` |

**Asynq Task Definitions:**

| Task | Queue | Max Retries | Backoff | Dead-letter Action |
|---|---|---|---|---|
| `coa:send` | `network_commands` | 5 | Exponential (2s base) | Alert + dead-letter |
| `pod:send` | `network_commands` | 5 | Exponential (2s base) | Alert + dead-letter |
| `notif:fup_warning` | `notifications` | 3 | Fixed 5 min | Log and discard |
| `notif:fup_throttle` | `notifications` | 3 | Fixed 5 min | Log and discard |
| `dunning:remind` | `notifications` | 3 | Fixed 5 min | Log and discard |
| `dunning:throttle` | `network_commands` | 5 | Exponential | Alert + dead-letter |
| `dunning:suspend` | `network_commands` | 5 | Exponential | Alert + dead-letter |

**Key Metrics:**
- `fup_breaches_total` (counter, label: `plan_id`)
- `fup_warnings_80pct_total` (counter)
- `coa_ack_total` (counter, labels: `result=ack|nak|timeout`)
- `dead_letter_queue_depth` (gauge) — PagerDuty if > 0

---

## 4.3 Module 3: Transactional Billing & Tax
**Module ID:** MOD-BIL | **SAD Ref:** SAD-COMP-004 | **FR:** FR-BIL-001..007

`internal/billing` — GST invoices with `decimal.Decimal`, dunning state machine, idempotent wallet, HMAC webhook validation, Gotenberg PDF, GSTR-1 export.

**Dunning State Machine:**
```
ACTIVE
  → [T-7d]    REMINDED_7       (WhatsApp + SMS + email)
  → [T-3d]    REMINDED_3       (WhatsApp + SMS)
  → [T-1d]    REMINDED_1       (WhatsApp + SMS)
  → [T+0]     GRACE_PERIOD
  → [T+24h]   SOFT_SUSPENDED   (CoA throttle + WhatsApp + SMS with payment link)
  → [T+72h]   HARD_SUSPENDED   (PoD + WhatsApp + SMS)
  → [Recharge → any state] → ACTIVE  (CoA restore + WhatsApp + SMS confirmation)
```
All transition notifications must pass DND check (FR-NOTIF-008).

**GSTR-1 Export (FR-BIL-006):** Monthly export job producing JSON/CSV with:
- B2B invoices (GSTIN-registered businesses): invoice-level detail
- B2C invoices (residential): state-wise aggregate
- HSN/SAC summary
- Nil-rated / exempt supplies if applicable

**Invoice PDF — Plain Language (FR-BIL-007):** Every invoice PDF must include a usage summary block:
```
Data used this cycle:  2,847 GB of 3,300 GB included
Speed applied:         100 Mbps / 100 Mbps (full speed)
```

**Key Metrics:**
- `wallet_recharge_total` (counter, labels: `method=razorpay|bbps|cash|manual`)
- `webhook_hmac_failures_total` (counter, labels: `provider=razorpay|bbps`)
- `dunning_transitions_total` (counter, labels: `to_state`)
- `invoice_generation_duration_seconds` (histogram)

---

## 4.4 Module 4: Asynq Background Tasks & Worker Orchestration
**Module ID:** MOD-TASK | **SAD Ref:** SAD-COMP-003 | **FR:** FR-FUP-002, FR-NOTIF-001..011, FR-REV-002

`internal/tasks` — 75-worker Asynq pool. Bulk PostgreSQL COPY flush (every 300s), dead-letter monitor (poll every 30s → PagerDuty if depth > 0), PII re-encryption rotation (90-day), invoice PDF generation, notification dispatch, revenue reconciliation.

**PII Re-encryption Safety:** Batch size 500, transactional commit per batch, resumable on failure, skips already-rotated records via `key_version_id` comparison.

---

## 4.5 Module 5: RBAC & API Security Middleware
**Module ID:** MOD-AUTH | **SAD Ref:** SAD-COMP-006 | **FR:** FR-SEC-005

`internal/middleware` — JWT validation + role enforcement at HTTP handler layer. Emits structured audit log on all state-modifying calls (actor, role, action, target, timestamp, correlation_id).

**Role Matrix:**

| Role | Subscribers | Sessions/CoA | Billing/Wallet | Tickets | LEA | Franchise | Revenue Dashboard |
|---|---|---|---|---|---|---|---|
| `noc_engineer` | Read | Read/Write | — | Read | With flag | — | — |
| `billing_admin` | Read/Write | — | Full | Read | — | — | Full |
| `csr` | Read | Read | Read | Read/Write | — | — | — |
| `technician` | Read | Read | — | Read/Write | — | — | — |
| `lco_partner` | Own only | Own only | Own only | Own only | — | Own | — |

---

## 4.6 Module 6: CGNAT & LEA Export
**Module ID:** MOD-CGNAT | **SAD Ref:** SAD-COMP-004 | **FR:** FR-NET-001..003

`internal/cgnat` — Records port-block allocations, provides LEA lookup API, writes tamper-evident audit record on every lookup. Access restricted to `noc_engineer` + `lea_access` claim.

---

## 4.7 Module 7: Notification Service (WhatsApp + SMS + Email) *(new — gap CRD-NOTIF-001)*
**Module ID:** MOD-NOTIF | **SAD Ref:** SAD-COMP-005 | **FR:** FR-NOTIF-001..011

`internal/notifications` — Dedicated dispatcher invoked exclusively by Asynq tasks. Never called synchronously from the request path.

**WhatsApp Business API Integration:**
- Provider: Meta Cloud API (v17+)
- Authentication: Bearer token from secret manager; rotated every 30 days
- Message type: Template messages only (pre-approved via Meta Business Manager)
- Template storage: `notification_templates` table with `channel`, `template_id`, `template_name`, `variables_schema`
- Delivery callback endpoint: `POST /webhooks/whatsapp` — receives `sent`, `delivered`, `read`, `failed` status updates; updates `notification_log.delivery_status`

**Template Catalogue:**

| Template ID | Event | Variables | Channel |
|---|---|---|---|
| `TMPL-001` | FUP 80% warning | `{{subscriber_name}}`, `{{gb_used}}`, `{{gb_total}}`, `{{plan_name}}` | WhatsApp + SMS |
| `TMPL-002` | FUP throttle applied | `{{subscriber_name}}`, `{{speed_reduced_to}}`, `{{payment_link}}` | WhatsApp + SMS |
| `TMPL-003` | Renewal reminder | `{{subscriber_name}}`, `{{days_left}}`, `{{amount}}`, `{{payment_link}}` | WhatsApp + SMS + Email |
| `TMPL-004` | Soft suspension | `{{subscriber_name}}`, `{{reason}}`, `{{payment_link}}` | WhatsApp + SMS |
| `TMPL-005` | Hard suspension | `{{subscriber_name}}`, `{{contact_number}}` | WhatsApp + SMS |
| `TMPL-006` | Service restored | `{{subscriber_name}}`, `{{plan_name}}`, `{{expiry_date}}` | WhatsApp + SMS |
| `TMPL-007` | Payment received | `{{subscriber_name}}`, `{{amount}}`, `{{transaction_id}}`, `{{new_expiry}}` | WhatsApp + SMS |
| `TMPL-008` | Ticket update | `{{subscriber_name}}`, `{{ticket_id}}`, `{{status}}` | WhatsApp |

**DND Check Flow:**
```
Notification task arrives
  → Load subscriber.dnd_opt_out from DB
    → [TRUE]  → Skip all marketing/reminder channels
               → Allow payment receipts and service restoration (transactional class)
               → Write notification_log with status='suppressed_dnd'
    → [FALSE] → Proceed with dispatch
```

**Notification Log Record (FR-NOTIF-009):**
Every dispatch attempt writes a `notification_log` row regardless of outcome. Fields: `subscriber_id`, `channel`, `template_id`, `triggered_by_event`, `triggered_by_entity_id`, `sent_at`, `delivery_status`, `failure_reason`, `provider_message_id`.

**Key Metrics:**
- `notification_dispatch_total` (counter, labels: `channel`, `template_id`, `status=sent|failed|suppressed`)
- `notification_delivery_latency_seconds` (histogram, labels: `channel`)
- `whatsapp_delivery_status_total` (counter, labels: `status=delivered|read|failed`)

---

## 4.8 Module 8: Revenue Assurance *(new — gap BO-001)*
**Module ID:** MOD-REV | **SAD Ref:** SAD-COMP-009 | **FR:** FR-REV-001..004

`internal/revenue` — Nightly Asynq job + on-demand API endpoint.

**Unbilled Subscriber Report (FR-REV-001):**
```sql
-- Subscribers active but with no invoice in current billing cycle
SELECT s.id, s.username, s.plan_expiry, s.wallet_balance
FROM subscribers s
LEFT JOIN invoices i ON i.subscriber_id = s.id
  AND i.created_at >= date_trunc('month', NOW())
WHERE s.status = 'active'
  AND i.id IS NULL;
```

**Ledger Reconciliation (FR-REV-002):**
```sql
-- Must return zero variance
SELECT
  SUM(s.wallet_balance)                            AS system_balance_total,
  SUM(CASE WHEN wl.entry_type='credit' THEN wl.amount ELSE 0 END)
  - SUM(CASE WHEN wl.entry_type='debit'  THEN wl.amount ELSE 0 END) AS ledger_net,
  ABS(SUM(s.wallet_balance) - (
    SUM(CASE WHEN wl.entry_type='credit' THEN wl.amount ELSE 0 END)
    - SUM(CASE WHEN wl.entry_type='debit' THEN wl.amount ELSE 0 END)
  ))                                               AS variance
FROM subscribers s
CROSS JOIN (SELECT entry_type, amount FROM wallet_ledgers) wl;
```

**Collections Forecast (FR-REV-004):** 30-day rolling window of subscribers with expiry dates, multiplied by plan price. Segments: will auto-renew (wallet ≥ plan price), at-risk (wallet < plan price), already lapsed.

---

## 4.9 Module 9: Subscriber Self-Service Portal *(new — gap PER-006)*
**Module ID:** MOD-PORTAL | **SAD Ref:** SAD-COMP-010 | **FR:** FR-SUB-001..005

`web/portal` — Responsive web app (server-side rendered or React SPA). Authenticated via subscriber-scoped JWT (separate from admin JWT; contains `subscriber_id` claim only, no admin roles).

**Portal Pages:**

| Page | Data Source | FR |
|---|---|---|
| Dashboard | Real-time usage from Redis, plan/wallet from DB | FR-SUB-001..002 |
| Usage history | `subscriber_session_history` monthly aggregate | FR-SUB-001 |
| Invoices & payments | `invoices`, `wallet_ledgers` | FR-SUB-002 |
| Renew plan | Razorpay/BBPS deeplink; idempotent `transaction_token` | FR-SUB-003 |
| Support tickets | `tickets` table; create + view status | FR-SUB-004 |
| Notification history | `notification_log` filtered by subscriber | FR-SUB-005 |

**Real-Time Usage Display:** Portal polls `GET /api/v1/subscribers/{id}/usage` (reads from Redis, not DB) every 60 seconds. Displays: GB used, GB remaining, FUP status, speed profile.

---

## 4.10 Module 10: Franchise / LCO Module *(new — gap BO-004)*
**Module ID:** MOD-FRN | **SAD Ref:** SAD-COMP-011 | **FR:** FR-FRN-001..003

`internal/franchise` — Multi-tenant isolation via `franchise_id` on subscriber, invoice, and ledger tables.

**LCO Commission Flow:**
1. Subscriber recharge event fires
2. `commission:calculate` Asynq task runs, applies LCO commission rate (configurable per LCO)
3. Credit entry posted to `lco_ledger` (separate from subscriber `wallet_ledger`)
4. Parent ISP dashboard aggregates across all LCOs for consolidated P&L

**Data Isolation:** Every DB query from LCO-scoped JWT automatically includes `AND franchise_id = {caller_franchise_id}` via middleware row-filter. LCO users cannot access other franchises' data even with direct API calls.
