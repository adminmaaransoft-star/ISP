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
