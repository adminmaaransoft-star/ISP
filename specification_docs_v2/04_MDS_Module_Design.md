# Document 4: Module Design Specification (MDS)
**Version:** 2.2 | **Status:** Draft | **Date:** 2026-08-12 — §4.13 added (CRD §1.11 Phase 3); §4.11–4.12 unchanged from v2.1, §4.1–4.10 unchanged from v2.0
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

---

## 4.11 Module 11: Multi-Vendor NAS Attribute Engine *(new — gap CRD-EXP-001, FR-NAS-001..004)*
**Module ID:** MOD-NAS | **SAD Ref:** extends SAD-COMP-001 (AAA Control Plane Daemon) — new SAD-COMP entry pending a dedicated SAD pass | **FR:** FR-NAS-001..004

### Why this module exists

`internal/radius/handlers.go` and `internal/fup/coa_task.go` today hand-encode
one hardcoded MikroTik VSA (vendor 14988, attribute 8) into every
Access-Accept and CoA packet, regardless of what actually sent the request.
Any Cisco, Juniper, Huawei, ZTE, or wireless-controller NAS authenticates
subscribers correctly but receives an attribute it doesn't understand — the
subscriber connects at whatever the NAS's own default is, not their plan
speed, with no error anywhere because RADIUS silently ignores unrecognized
vendor attributes by design. This module replaces the single hand-rolled
builder with a per-NAS vendor strategy, without touching the working
MikroTik path (which becomes the reference implementation of the same
interface, and remains the fallback default — see rollout note below).

### Two fundamentally different attribute models

The vendor split isn't just "different attribute numbers" — it's two
different provisioning models, and the design has to carry both:

| Model | Vendors (attribute family — **verify against deployed firmware before relying on exact numbers**) | What RADIUS sends |
|---|---|---|
| **Dynamic numeric rate** | MikroTik (vendor 14988, `Mikrotik-Rate-Limit`, already implemented) · Huawei (vendor 2011, `Huawei-Input-Average-Rate` / `Huawei-Output-Average-Rate`) · ZTE (vendor-specific rx/tx rate pair, model-dependent) | A literal bps/rate value computed from `plans.rate_limit_string` at request time — no NAS-side pre-configuration needed per plan |
| **Policy/profile reference** | Cisco (vendor 9, `cisco-avpair`, e.g. `subscriber:sub-qos-policy-in=<name>`) · Juniper (named firewall filter / hierarchical policer reference, `Filter-Id` or vendor-specific) · Wireless controllers — Cisco WLC (vendor 14179, `Airespace-Data-Bandwidth-Average-Contract`), Aruba (vendor 14823, role/QoS reference), Ruckus (vendor-specific role reference) | A **name** the NAS resolves against a QoS policy/profile it already has provisioned locally — RADIUS never sends a raw number to these vendors |

The practical consequence: for reference-vendors, a plan tier must exist as a
matching named policy on the NAS *before* RADIUS can select it. That's an
operational/runbook dependency (OPS §12, to be added when this module is
scheduled), not something this module can provision remotely — CWMP/TR-069
(FR-CPE, Phase 4) provisions the CPE, not the NAS's own QoS policy-map table.

### Interface

```go
// internal/nas — new package
type RateProfile struct {
    RateLimitString string // "50M/50M" — plans.rate_limit_string, source for dynamic-rate vendors
    ProfileName     string // pre-provisioned NAS-side policy name, source for reference vendors
}

type AttributeBuilder interface {
    BuildAccept(p RateProfile) ([]*radius.Attribute, error) // Access-Accept
    BuildCoA(p RateProfile) ([]*radius.Attribute, error)    // CoA-Request
    // PoD carries no vendor attribute (RFC 3576 Disconnect-Request needs
    // only Acct-Session-Id) — not part of this interface.
}

var builders = map[Vendor]AttributeBuilder{
    VendorMikrotik: mikrotikBuilder{}, // wraps the existing, unmodified VSA logic
    VendorHuawei:   huaweiBuilder{},
    VendorZTE:      zteBuilder{},
    VendorCisco:    ciscoBuilder{},
    VendorJuniper:  juniperBuilder{},
    VendorWireless: wirelessBuilder{},
}
```

`internal/radius/handlers.go` (Access-Accept) and `internal/fup/coa_task.go`
(CoA) both call `nas.BuilderFor(vendor).BuildAccept/BuildCoA(profile)` instead
of constructing the VSA inline. `internal/fup/pod_task.go` is unchanged — PoD
needs no vendor-specific attribute.

### Vendor resolution and rollout safety

Vendor is resolved by looking up the requesting NAS's source IP (Access-
Request) or the session's recorded NAS IP (CoA, via the existing
`FUPStore.GetSubscriberNASSession` lookup) against the new `nas_devices`
table (DBD §6.2). **An IP with no matching row defaults to
`VendorMikrotik`** — this is deliberate, not an oversight: it reproduces
today's actual behavior exactly, so an existing MikroTik-only deployment
needs zero new rows to keep working after this ships. A
`nas_unclassified_total{nas_ip}` counter increments on every such fallback,
giving NOC an actionable list of NAS devices worth registering rather than a
silent assumption.

### Per-NAS RADIUS secret

Today's `internal/radius/daemon.go` verifies every packet against one global
`RADIUS_SECRET`. Per-NAS secrets require the packet server's secret lookup
itself to become IP-aware — `layeh.com/radius`'s `PacketServer.SecretSource`
accepts a `func(ctx, *net.UDPAddr) ([]byte, error)` precisely for this case.
Resolution order: `nas_devices` row for the source IP (decrypted — see DBD
§6.2) → fall back to the existing global `RADIUS_SECRET` if no row exists,
same backward-compatible default as vendor resolution above.

### Key Metrics

- `nas_unclassified_total` (counter, label: `nas_ip`) — NAS traffic seen with
  no `nas_devices` row, currently served on the MikroTik-fallback default
- `nas_attribute_build_errors_total` (counter, labels: `vendor`, `reason`) —
  e.g. a reference-vendor plan with no matching `plan_nas_profiles` row
- `radius_auth_total` gains a `nas_vendor` label (extends the existing
  metric from §4.1, not a new one)

---

## 4.12 Module 12: PostgreSQL High Availability & Failover *(new — gap CRD-EXP-001, NFR-AVAIL-002)*
**Module ID:** MOD-PGHA | **SAD Ref:** extends SAD-COMP-004 (Relational Storage Core) | **FR:** NFR-AVAIL-002

### Why this module exists

Redis has real Sentinel HA today (3 sentinels, quorum 2, tested failover —
IDD §8.3). PostgreSQL — where `subscribers`, `wallet_ledgers`, and every
billing record live — is a single container with no replica and no
failover. A crashed or corrupted primary is a full outage with a restore-
from-backup RTO, not a supervised promotion. This module's scope is
deliberately the **application-layer contract** a failover needs (connection
behavior, what's safe to route to a replica); the deployment topology itself
(which failover manager, how many standbys, DNS/VIP mechanics) belongs in
IDD §8 and is a separate infrastructure design pass — noted here as a
dependency, not designed in this document.

### Application-layer failover contract

- **Connection string carries both hosts.** `pgx`'s multi-host DSN
  (`host=pg_primary,pg_standby port=5432,5432 target_session_attrs=read-write`)
  lets the driver itself find the current primary after a promotion, rather
  than the application needing to watch for a failover event. `db.Connect`
  (`internal/db`) takes this DSN form; no application code changes on
  failover, only the DSN config.
- **Retry-with-backoff on connection-layer errors**, distinct from query
  errors: a promotion takes a real, bounded window (seconds, not
  milliseconds) during which every connection attempt fails. The existing
  Asynq retry pattern (`internal/fup`, `internal/billing` — exponential
  backoff, max 5 attempts) is the template; this module applies the same
  shape to the DB connection pool's own reconnect logic rather than
  inventing a second retry convention.
- **RADIUS auth path must fail closed, not open, on a DB outage during
  failover.** The subscriber cache (`internal/cache/subscriber_cache.go`)
  already serves reads from Redis with a 60s TTL and Postgres only on a
  cache miss — during a short promotion window, already-cached subscribers
  keep authenticating normally and only *new* logins or cache-miss re-auths
  are affected. This existing cache-first design is what keeps a Postgres
  failover from being a RADIUS outage, and should not be weakened while
  adding HA.

### Read-routing candidates (once a standby exists)

Not every read needs the primary. Reports that tolerate replica lag are
routing candidates, cutting primary load without a schema change:

| Query | Current source | Replica-safe? |
|---|---|---|
| Unbilled-subscriber report (FR-REV-001) | Primary | Yes — nightly batch, lag-tolerant |
| Ledger reconciliation (FR-REV-002) | Primary | **No** — must read a consistent point-in-time snapshot; replica lag would produce false variance alerts |
| Revenue/collections dashboards (staffui) | Primary | Yes — dashboard, not a transactional read |
| RADIUS auth fallback on cache miss | Primary | **No** — must read the current, not-lagged, subscriber state (status, plan) |
| CoA/PoD NAS/session lookup | Primary | **No** — same currency requirement as auth |

The rule of thumb this table encodes: anything that gates a live decision
(auth, CoA target, ledger truth) stays on the primary; anything that
summarizes history for a human is a replica candidate.

### Key Metrics

- `pg_replication_lag_seconds` (gauge, from `pg_stat_replication` on the
  primary) — feeds an OPS alert threshold (to be set in a later OPS pass)
- `db_connection_retry_total` (counter, labels: `outcome=recovered|exhausted`)
- `db_failover_detected_total` (counter) — incremented when the pool
  observes a primary-target change mid-session

---

## 4.13 Module 13: Helpdesk & SLA Engine *(new — gap CRD-EXP-002, FR-SUP-001..003)*
**Module ID:** MOD-SUP | **SAD Ref:** new component, extends the ticket write
paths already covered informally under SAD-COMP-006 (API Gateway & RBAC) —
a dedicated SAD-COMP entry is pending a full SAD pass | **FR:** FR-SUP-001..003

### What exists today, and the gap

`tickets` (migration 009) has `category`, `status`, and a bare, **unconstrained**
`assigned_to INTEGER` — the migration's own comment promises "FK to
admin_users.id added in future migration"; that migration was never written,
and `admin_users` never existed. The real staff table (`staff_users`,
migration 021) postdates the ticket table by twelve migrations. No priority,
no due date, no breach tracking, no index beyond the primary key — a ticket
created a week ago and one created a minute ago are indistinguishable in a
list query without reading every row's `created_at`. Three separate call
sites write to this table today (`internal/api/tickets.go`,
`internal/staffui/screens.go`, `internal/portal` for subscriber-raised
tickets) and none of them sets anything SLA-related, because there is
nothing to set.

### Design decisions, and why

**Priority is category-derived by default, staff-overridable, subscriber
never sets it directly.** A subscriber choosing "critical" for every ticket
is not a hypothetical — it is the default outcome of letting the reporter
set their own urgency. `category` already carries a real urgency signal
(`connectivity` — the subscriber has no service — is categorically more
urgent than `plan_change`), so priority defaults from a category → priority
table staff can retune without a deploy, and only staff (console/API) can
override it after triage. Portal-created tickets always get the default.

**SLA has two clocks, not one: response and resolution.** "Time until
someone even looks at this" and "time until this is actually resolved" are
different operational signals — a ticket sitting at `open` past its
response SLA means nobody has started; one past its resolution SLA but
already `in_progress` is a different problem. `FR-SUP-001`'s "a computed
SLA due-by timestamp" becomes two: `sla_response_due_at` (breached if still
`open` when it passes) and `sla_resolution_due_at` (breached if not
`resolved`/`closed` when it passes).

**SLA targets live in a table (`sla_policies`), not Go constants.** Same
reasoning as `plan_nas_profiles` in §4.11: an ops team retuning "how fast
must a critical connectivity ticket be resolved" is a data change, not a
code change. Keyed on `(category, priority)`, not priority alone — a
critical billing dispute and a critical connectivity outage plausibly
deserve different resolution windows even at the same priority label.

**Due-by timestamps are a snapshot at creation, not a live recomputation.**
Both are computed once, from the ticket's `created_at`, at insert time (and
recomputed, still anchored to the *original* `created_at`, if staff change
priority during triage). Anchoring to a floating "last touched" time instead
would let repeated updates push a deadline out indefinitely — the same
"snapshot, not live" reasoning already applied to CoA/PoD's NAS-session
lookup happening at task-execution time rather than trusting a stale
enqueue-time payload (MDS §4.2), just in the opposite direction: there, the
snapshot is deliberately *not* trusted; here, it deliberately *is*, because
letting it drift is the bug.

**Breach and warning events are a log, not columns.** Four extra booleans/
timestamps on the hot `tickets` table (`response_warned_at`,
`response_breached_at`, `resolution_warned_at`, `resolution_breached_at`)
would work, but `sla_events` (ticket_id, event_type, occurred_at,
`UNIQUE(ticket_id, event_type)`) is the same append-only-log shape this
codebase already uses for `notification_log` and `lea_audit_log`, and the
uniqueness constraint *is* the idempotency mechanism the scanner needs
(insert, check whether a row was actually written, only alert if so) rather
than a hand-rolled "have I already warned about this" check.

**Warning threshold: 80% of the window elapsed** — reusing FR-FUP-004's
already-shipped 80%-warning pattern for FUP quota exactly, not inventing a
second convention for the same shape of problem.

**Routing targets a role, not a specific staff member.** Auto-assigning to
an individual needs a workload/availability model this codebase has no data
for (nothing tracks how many open tickets a given `staff_users` row already
has). `ticket_routing_rules` (`category` nullable, `franchise_id` nullable,
`target_role`, `priority_order`) resolves to a role at creation time,
stored on the ticket (`routed_role`) — a human still picks the specific
assignee from that role's queue. Rule matching is explicit-precedence, not
inferred specificity: lowest `priority_order` among rules whose nullable
columns match (or are null, meaning "any") wins; no match leaves
`routed_role` null and the ticket in the general queue.

### Write-path integration

`internal/db/tickets.go`'s `CreateTicketAdmin` (and the portal's
`PortalStore.CreateTicket`, `internal/db/subscribers.go`) both gain the same
three-step sequence before insert:

```sql
-- 1. Resolve priority (skip if caller supplied an explicit override — staff only)
--    Category → default priority is itself a small lookup table
--    (category_priority_defaults) rather than a Go switch, for the same
--    retune-without-deploy reason as sla_policies.
SELECT default_priority FROM category_priority_defaults WHERE category = $1;

-- 2. Resolve SLA targets for (category, resolved priority)
SELECT response_minutes, resolution_minutes FROM sla_policies
WHERE category = $1 AND priority = $2;

-- 3. Resolve routing (first match by priority_order; franchise_id comes from
--    a join to subscribers, since tickets does not carry it independently
--    except as the denormalized copy this module adds — see DBD)
SELECT target_role FROM ticket_routing_rules
WHERE (category = $1 OR category IS NULL)
  AND (franchise_id = $2 OR franchise_id IS NULL)
ORDER BY priority_order ASC LIMIT 1;
```

Then a single `INSERT` carries `priority`, `sla_response_due_at =
created_at + response_minutes`, `sla_resolution_due_at = created_at +
resolution_minutes`, `franchise_id`, and `routed_role`. A category/priority
pair with no `sla_policies` row is a configuration gap, not a silent
no-op: the insert should fail loudly (`NOT NULL` on the resolution columns
with no `DEFAULT`) rather than create a ticket with no SLA at all — the same
"never fail silently" stance FR-NAS's `nas_attribute_build_errors_total`
already takes (MDS §4.11) for a materially identical failure shape (a
lookup with no matching row).

`UpdateTicketAdmin`'s priority-change path recomputes step 2 and rewrites
both due-at columns from the ticket's existing `created_at` — never from
`now()`.

### SLA breach scanner

`internal/tickets/sla_scanner.go` (new file, existing package — the one
`notify_task.go` already lives in). Same shape as `fup.Scanner` and
`billing.DunningScanner` (MDS §4.2, §4.3): a ticker-driven loop registered
in `cmd/radiusd/main.go` alongside them, not a new pattern.

```go
type SLAScanner struct {
    db     SLAQuerier
    client *asynq.Client
}

func (s *SLAScanner) Run(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Minute) // SLA windows are hours, not seconds — FUP's 10s cadence is the wrong reference point here; billing's hourly scan is the closer analog, halved for tighter breach detection
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.scan(ctx)
        }
    }
}
```

Each tick, for both response and resolution clocks independently: find open
tickets past the 80%-warning threshold or past the due-at itself, attempt
`INSERT INTO sla_events (ticket_id, event_type) VALUES (...) ON CONFLICT
(ticket_id, event_type) DO NOTHING`, and only enqueue an alert task when
that insert actually added a row (`RowsAffected() == 1`) — the same
insert-and-check-rows-affected idempotency shape already used for dead-letter
alerting (`internal/fup/deadletter_monitor.go`).

### Alerting: dashboard first, reusing the existing Alerter — not a new channel

FR-SUP-002 asks for "dashboard + notification." The dashboard half is new
UI work (a breach count/badge on staffui's Support section, `internal/
staffui/screens.go`) — out of scope for this schema/module pass, tracked
for the implementation pass. The notification half deliberately does **not**
introduce a new staff notification channel: `staff_users` has no email or
phone column today, and building one now would be scope creep into
FR-NOTIF-012 (email channel, Phase 4, not yet implemented) or a staff-facing
SMS/WhatsApp channel that doesn't exist either. Instead, a breach event
enqueues through the same `Alerter` interface (`Trigger(event string, detail
any)`) `fup.DeadLetterMonitor` already depends on and `cmd/radiusd/main.go`
already wires to `logAlerter{}` (PagerDuty-shaped, log-only until PagerDuty
delivery is actually implemented — an existing, already-documented
limitation, not a new one this module introduces). A real per-staff
notification is a real future need; it is not manufactured here just to
check a box.

### Key Metrics

- `sla_events_total` (counter, labels: `event_type`) — mirrors
  `fup_breach_detected_total`'s shape (MDS §4.2)
- `sla_scan_duration_seconds` (histogram) — the scan touches every open
  ticket every 5 minutes; worth watching as ticket volume grows
- `tickets_unrouted_total` (gauge) — tickets with no matching
  `ticket_routing_rules` row, i.e. sitting in the general queue with no
  role signal at all

## 4.14 Module 14: Billing Lifecycle — Auto-Renewal, Adjustments, Refunds & Subscriber Lifecycle *(new — gap BO-007, "subscriber lifecycle management, invoice generation, recurring billing, and payment adjustments")*

**Module ID:** MOD-BILLC | **SAD Ref:** SAD-COMP-004 (extends MOD-BIL) | **FR:** FR-BIL-008..011, FR-LC-001..003

### What exists today, and the gaps

Three pieces of infrastructure this module depends on were already built and
tested in isolation, each with a caller missing exactly the way
`DunningScanner` was missing before MDS §4.13:

1. `billing.CalculateGstInvoice` and `db.BillingStore.CreateInvoice` compute
   and persist a correct GST invoice — but nothing in the renewal path calls
   them. A subscriber can renew via the portal (`portal.RenewalProcessor`)
   and receive service, with no invoice ever created for that charge.
2. `billing.WalletService.Recharge` posts a double-entry credit — but the
   only way a subscriber's wallet balance is ever consumed is a direct,
   side-effect-free `SET wallet_balance` that does not exist yet either: no
   code path debits the wallet for a plan renewal. Renewal today is entirely
   "top up the wallet, then separately push `plan_expiry` out" — never "pay
   for the plan out of what's already there."
3. `billing.NextDunningState` is, by design, a pure function of `plan_expiry`
   vs. `now` (MDS §4.13's design note), with no awareness of
   `wallet_balance`. That decoupling is correct for what dunning does — but
   it means a subscriber who tops up their wallet with more than enough to
   cover their next cycle, then does nothing else, is suspended on schedule
   anyway. Nothing converts "has the money" into "renewed."

Separately, `UpdateSubscriber` (`internal/db/subscribers.go:167`) —
the handler behind `PATCH /api/v1/subscribers/{id}` — accepts a bare
`plan_id` change and applies it as a single `SET` with zero side effects: no
proration, no Redis auth-cache invalidation, no CoA. This is the exact gap
SRS FR-AAA-007 already specifies ("A plan change or top-up must invalidate
the subscriber's Redis auth-cache entry and enqueue a CoA... so the new rate
limit applies without waiting for reauthentication") but which no code path
actually implements. This module closes it via a dedicated endpoint rather
than by changing `PATCH`'s existing contract (see below).

### Scope note: not Razorpay auto-charge

The CRD is explicit that v1 is prepaid-only; there is no stored payment
instrument and no Razorpay auto-debit mandate anywhere in this codebase.
"Recurring billing" here means **auto-renewal from an existing wallet
balance** — if a subscriber has already topped up enough to cover their next
cycle, the system renews them automatically from that balance rather than
making them take a manual action, or worse, suspending them while their
money sits uncharged in the wallet. This is both a real reading of what
"recurring billing" can mean in a prepaid-wallet architecture, and a genuine
correctness gap (item 3 above) that predates this module.

### Design decisions, and why

**Invoice creation is folded into every path that extends `plan_expiry`
through a wallet debit — portal renewal and the new auto-renewal scanner
— not into `WalletService.Recharge` itself.** `Recharge` is also used for
plain wallet top-ups that are not, by themselves, a renewal (a subscriber
topping up ahead of when they intend to use it). Invoicing at the recharge
boundary would create an invoice for money that has not yet paid for
anything. Invoicing at the point `plan_expiry` actually advances ties the
invoice to the thing it is supposed to represent: one cycle of service.

**A new `WalletService.Post` method is added alongside `Recharge`, not
inside it.** `Recharge` is FR-BIL-003/005's tested, idempotent, always-credit
entry point and is left untouched. `Post` is the general primitive
auto-renewal debits, staff adjustments, and refunds all need: an arbitrary
direction (credit or debit) against `AccountSubscriberWallet`, with a
caller-supplied counter account. It reuses the existing
`WalletQuerier.RecordRecharge` DB primitive unchanged — that method already
takes an arbitrary debit/credit leg pair and writes them atomically with the
new balance; nothing about it is actually recharge-specific except the name.

**Two new ledger accounts, not a parallel taxonomy.** `wallet_ledgers.account`
gains `revenue_clearing` (the counter-leg for a plan charge consumed from the
wallet — auto-renewal or, in a future pass, staff-recorded cash renewal) and
`adjustment_clearing` (the counter-leg for staff-issued credits, debits, and
refunds). `(account, entry_type, description)` already disambiguates every
posting; a separate `reason`/`source` enum column would duplicate that.

**A DB-level `CHECK (wallet_balance >= 0)` backstops the application-level
balance check.** `Post`'s debit path reads the balance, computes the new
one, and rejects the request with `ErrInsufficientBalance` before writing —
but that read-then-write is not itself atomic against a second concurrent
debit on the same subscriber (auto-renewal scanner and a staff adjustment
racing, for instance). The application check is the normal path (a clean
422); the CHECK constraint is what makes an overdraft actually impossible
under a race, at the cost of that rare case surfacing as a 500 instead of a
422. Both are cheap and neither replaces the other.

**Auto-renewal restores `dunning_state` by extending `plan_expiry`, not by
writing `dunning_state` directly.** Same reasoning as MDS §4.13: one
scanner (`DunningScanner`) owns every transition of that column. Extending
`plan_expiry` is enough — `NextDunningState` already computes `active` for a
subscriber whose expiry has been pushed back into the future, so the very
next `DunningScanner.Scan` walks them home.

**Fixing `dunning.go` while verifying that path: `remind_7d`/`remind_3d`/
`remind_1d` had no restore edge to `active` in `validTransitions`.**
`NextDunningState` computes `active` as the correct target for a `remind_*`
subscriber whose `plan_expiry` moved back out past 7 days (the switch
statement's fall-through path, not its explicit restore branch, which only
lists `grace_period`/`soft_suspended`/`hard_suspended`). `stepToward`
special-cases any restore as a single hop straight to `active` regardless of
current state, and `TransitionDunning` then rejects that hop for a `remind_*`
subscriber because the edge was never listed — `advance` logs the error and
the subscriber's `dunning_state` sticks at `remind_7d` forever. `status`
stays `active` throughout (`dunningToSubscriberStatus` maps all three remind
states to `active`), so this was invisible to any user-facing check — it
only ever showed up as a stuck cosmetic value and a harmless recurring log
error. It predates this module, but the new auto-renewal restore path
exercises exactly this edge, so it is fixed here rather than left for a
scanner that would otherwise error every single hour for any subscriber who
renews while still in a reminder stage. Added: `remind_7d → active`,
`remind_3d → active`, `remind_1d → active`.

**Plan change is a dedicated endpoint (`POST
/subscribers/{id}/plan-change`), not an extension of `PATCH
/subscribers/{id}`.** The existing `PATCH` is already used for plain
corrections (fixing a misrecorded `plan_id`, flipping `status` outside the
dunning lifecycle) with call sites and tests that expect zero side effects.
Overloading it to sometimes prorate, invalidate cache and fire a CoA — based
on which field changed — makes its behavior conditional on intent that isn't
visible in the request. A new endpoint makes "this changes what the
subscriber owes and their live session" an explicit, auditable action
instead of an inferred one, and closes FR-AAA-007 without touching `PATCH`'s
existing contract.

**Proration formula.** On a plan change from old→new with `now`:

```
remaining_days   = max(0, plan_expiry - now) in days
old_daily_value  = old_plan.price / old_plan.validity_days
credit           = remaining_days * old_daily_value      // unused value of the old plan
new_daily_value  = new_plan.price / new_plan.validity_days
bonus_days       = floor(credit / new_daily_value)
new_plan_expiry  = now + new_plan.validity_days + bonus_days
```

The subscriber always gets the new plan's full validity from the moment of
the change; unused value from the old plan is converted to bonus days on the
new plan rather than a separate cash refund, since a staff-initiated plan
change is not a payment event and there is no amount collected to refund
against. A downgrade with no remaining old-plan value simply grants the new
plan's own validity with zero bonus days.

**Termination is a dedicated endpoint, not a `status` value reachable
through `PATCH`.** `terminated` was already a legal value in the
`subscribers.status` CHECK (migration 003) with no code path that ever wrote
it and no PoD triggered when it did. Termination is irreversible and
disconnects a live session (PoD, not CoA — the subscriber is not getting a
new rate limit, they are leaving), which is different enough in
consequence from every other status transition that it gets its own
audited action rather than being one more value `PATCH` happens to accept.

**Refunds are tracked in their own table (`payment_refunds`), separate from
the `wallet_ledgers` posting that moves the money.** A refund is both a
ledger event (money left the wallet) and a business event with its own
lifecycle a wallet posting has no room to express — this deployment applies
it synchronously (no live Razorpay refund API integration exists), so every
refund is created with `status = 'processed'` immediately, but the column
exists so a future asynchronous gateway refund can move through
`requested → processed/failed` without a schema change. `payment_refunds`
carries a `ledger_entry_id` FK to the `wallet_ledgers` row it corresponds to,
so the two are always traceable to each other.

### Write-path integration

```
Portal renewal (existing) ─┐
                            ├─→ WalletService.{Recharge,Post} debits/credits
Auto-renewal scanner (new)─┘        │
                                     ▼
                          plan_expiry advances (renewal only)
                                     │
                                     ▼
                    CalculateGstInvoice → BillingStore.CreateInvoice
                                     │
                                     ▼
                     next DunningScanner.Scan restores dunning_state
```

```
Staff plan-change  → proration → SetSubscriberPlan(plan_id, plan_expiry)
                                → cache.InvalidateSubscriber(username)
                                → enqueue CoA (if an active session exists)

Staff terminate    → status = terminated
                                → enqueue PoD (if an active session exists)

Staff adjustment   → WalletService.Post (credit or debit, adjustment_clearing)
Staff refund       → WalletService.Post (debit, adjustment_clearing) + payment_refunds row
```

### RecurringBillingScanner

Mirrors `DunningScanner`'s shape exactly (`Run`/`Scan`, injectable clock,
Prometheus counters, per-item error logging that does not halt the batch)
and is wired to run on a *shorter* interval (15 minutes vs. dunning's hourly)
so it always gets a chance to renew a funded subscriber before the hourly
dunning tick would otherwise escalate them — though because `NextDunningState`
walks back to `active` from any suspended state once `plan_expiry` moves to
the future, a renewal that lands after an escalation self-heals on dunning's
next tick regardless of ordering.

Candidate query: `status != 'terminated' AND plan_expiry <= NOW() AND
wallet_balance >= plans.price` (joined on the subscriber's current plan).
Only subscribers who have *already* reached their expiry are renewed —
this is reactive top-up-triggered renewal, not early renewal, matching
`portal.Renew`'s existing "renew when it's actually due" behavior.

For each candidate: debit the plan price via `WalletService.Post`
(`revenue_clearing` counter-leg), extend `plan_expiry` by the same
`max(now, currentExpiry) + validity_days` rule `extendPlanExpiry` already
uses for portal renewal, create the invoice, and — since a subscriber can be
caught by this scanner while already `grace_period`/`soft_suspended`/
`hard_suspended` (e.g. the first run after deployment, for subscribers who
lapsed before auto-renewal existed) — call `TransitionDunning(..., active)`
directly rather than waiting up to an hour for the dunning scanner to notice.

### Key Metrics

- `billing_autorenewal_total` (counter, labels: `result` = renewed/
  insufficient_balance/error) — insufficient_balance is expected volume
  (most candidates the query considers), not a failure signal
- `billing_autorenewal_invoice_failures_total` (counter) — the wallet debit
  already committed by the time invoicing runs; a failure here needs
  reconciliation, not a retried debit
- `billing_lifecycle_actions_total` (counter, labels: `action` =
  plan_change/terminate/adjustment/refund) — staff-lifecycle action volume,
  the same shape as `billing_dunning_transitions_total`

## 4.15 Module 15: Task & Approval Workflows *(new — gap BO-007, FR-WFL-001..002)*

**Module ID:** MOD-WFL | **SAD Ref:** SAD-COMP-004 (extends MOD-BILLC) | **FR:** FR-WFL-001..002

### What this closes

CRD-EXP-002 asks for "second-approver sign-off" before a sensitive account
action takes effect — named examples: large wallet credits, plan downgrades,
termination. MDS §4.14 built exactly the three highest-stakes actions this
would gate (staff wallet credit, refund, termination) as immediate,
single-operator actions with no second party in the loop: a single
`billing_admin` token can move money or end an account unilaterally, with
only an audit-log entry after the fact. This module inserts a second,
distinct approver *before* the action executes, not just a record of it
having happened.

### Scope: which actions are gated, and why not more

Gated: **wallet credit adjustments**, **refunds**, and **termination** — the
three the task explicitly names. Debit adjustments are left ungated:
a debit only ever *reduces* what a subscriber can spend and is typically
itself a correction of an earlier erroneous credit, so gating it would add
friction to the safer direction of the same feature while leaving the
risky direction (crediting money, or ending service) exactly as gated as
before. Plan-change is left ungated too, even though CRD-EXP-002 mentions
"plan downgrades": FR-LC-001 (MDS §4.14) already computes the new
`plan_expiry` deterministically from both plans' price and validity — there
is no free-form amount an operator chooses, which is the specific risk a
second approver exists to catch. Extending the gate to plan-change is a
reasonable future step but not one this pass forces.

### Design decisions, and why

**A request-then-decide model, not a hold-and-notify one.** The three gated
endpoints (`POST /subscribers/{id}/adjustments` with `direction=credit`,
`/refunds`, `/terminate`) now create an `approval_requests` row and return
`202 Accepted` instead of performing the action. Nothing happens to the
wallet or the subscriber's status until a second, different staff member
calls `POST /approvals/{id}/approve`. This is the only way to honor "before
taking effect" literally — a design that executed first and asked for
sign-off after would be an audit trail, not an approval gate.

**The self-approval guarantee is enforced twice, not once.** The API handler
checks `decider != requested_by` before attempting anything; the schema
carries the identical rule as `CONSTRAINT chk_approval_distinct_approver`.
Neither is redundant: the app check produces a clean 403 for the normal
case, while the constraint is what makes self-approval structurally
impossible even from a future code path (a script, a different handler, a
bug) that forgets to check.

**Claiming a request is a single atomic conditional UPDATE, because two
approvers can race.** `ClaimApprovalRequest` runs `UPDATE approval_requests
SET status='approved', decided_by_username=$actor ... WHERE id=$1 AND
status='pending'`. Only one of two concurrent `/approve` calls on the same
request can match `status='pending'`; the loser sees zero rows affected and
is told the request was already decided, rather than both callers going on
to execute the underlying wallet debit or credit twice. Reject uses the
identical atomic claim, straight to `rejected`, for the same reason —
a reject racing an approve must not let both happen.

**`approved` is a persisted, not transient, intermediate status.** Between
the claim and the underlying action actually executing (`FinalizeApprovalExecution`
writing `executed` or `execution_failed`), a request can be observed sitting
at `approved`. This is deliberate: if the process crashes in that window,
the request is left in an honest, inspectable state — "someone approved
this and execution did not finish" — rather than either silently retried
(risking a double execution) or silently lost (the approver's decision
disappearing). Recovering a stuck `approved` row is an operational action,
not something this module automates, matching FR-BIL-009's "log for
reconciliation, do not auto-retry a money movement" precedent.

**Money-moving execution reuses `billing.WalletService.Post` and the
existing refund/lifecycle stores unchanged.** The approval flow is purely
what decides *whether and when* the action runs; it is not a parallel
implementation of what the action does. `wallet_ledgers.adjusted_by_username`
is set to the *requester*, with the approver's identity folded into the
ledger description (`"... (approved by X)"`) — the ledger attributes the
transaction to whoever's judgment call it fundamentally was, while the
`approval_requests` row itself is the complete, queryable record of both
parties for any dispute.

**Field-task assignment (FR-WFL-002) is a separate, much simpler table with
no approval gate of its own.** CRD's own wording — "independent of the
ticket system" — is the whole design brief: `field_tasks` is a flat
assign/track/complete record (`open → in_progress → completed/cancelled`)
with no SLA engine, no routing rules, and no relationship to
`approval_requests` beyond living in the same migration. Building it as an
extension of `tickets` would couple two features (subscriber-facing
support, and internal staff coordination) that the CRD is explicit about
keeping apart.

**Both new tables use free-form `*_username` columns, not `staff_users.id`
foreign keys.** Every JWT already carries the acting staff member's username
in `Subject` (`middleware.SubjectFromContext`) with no numeric staff id
anywhere in the claims. Resolving that to `staff_users.id` on every gated
call would be new lookup machinery this module does not otherwise need —
the same call MDS §4.14 already made for `wallet_ledgers.adjusted_by_username`
and `payment_refunds.refunded_by_username`, extended here for consistency.

### Write-path integration

```
POST /subscribers/{id}/adjustments (credit)  ─┐
POST /subscribers/{id}/refunds                ├─→ approval_requests (status=pending) ─→ 202
POST /subscribers/{id}/terminate              ─┘

POST /approvals/{id}/approve → ClaimApprovalRequest (atomic, self-approval blocked)
                              → executeApprovedAction (WalletService.Post / TerminateSubscriber)
                              → FinalizeApprovalExecution (executed | execution_failed)

POST /approvals/{id}/reject  → RejectApprovalRequest (atomic) — never executes
```

### Key Metrics

- `billing_lifecycle_actions_total` (MDS §4.14) gains two new label values
  per gated action — `*_requested` at creation and `*_approved` at
  execution — so the funnel from request to execution is visible without a
  new metric family.
- `workflow_approval_execution_failures_total` (counter, labels:
  `action_type`) — an approval that executed the underlying action and had
  it fail (e.g. balance moved between request and approval) is the one case
  where an operator must look, not just reconcile later.
