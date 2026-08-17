# Document 3: System Architecture Design (SAD)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** SAD
**Traces From:** [SRS](02_SRS_System_Requirements.md)
**Traces To:** [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md) → [IDD](08_IDD_Infrastructure_Design.md)

---

## 3.1 Architectural Style

The platform implements an asynchronous, decoupled, multi-tier architecture. The high-volume RADIUS control plane is fully isolated from the transactional relational storage layer through an intermediate caching and streaming pipeline.

**Design principles:**
- Cache-first auth: RADIUS never touches PostgreSQL on the hot path
- Async persistence: accounting flows Redis → stream → Asynq → PostgreSQL
- Observability by default: every tier emits Prometheus metrics and structured JSON logs
- Benefit-outcome traceability: every architectural decision maps to a business outcome in [CRD §1.1](01_CRD_Customer_Requirements.md)

---

## 3.2 Component Breakdown

### SAD-COMP-001 — AAA Control Plane Daemon
**Delivers:** FR-AAA-001..004, NFR-PERF-001, NFR-SCAL-001
**Detail:** [MDS §4.1](04_MDS_Module_Design.md), [DDS §5.1–5.3](05_DDS_Detailed_Design.md)

Go runtime executing `layeh.com/radius` bindings across a 128-worker bounded channel pool. Single master listener forwards UDP packets to workers. Workers authenticate from Redis, increment accounting atomically, and never touch PostgreSQL synchronously.

### SAD-COMP-002 — Cache & Event Streaming Tier (Redis 7.2 Sentinel HA)
**Delivers:** FR-AAA-002, FR-FUP-001, NFR-AVAIL-001 (≤8s failover — retargeted
2026-08-18 from an unachievable 3s; see TST §13.4)
**Detail:** [IDD §8.3](08_IDD_Infrastructure_Design.md)

3-node Sentinel cluster (1 primary, 2 replicas). Hosts subscriber session hashes, FUP usage sorted sets, deduplication keys, Asynq task queues, dead-letter queues, and rate-limiting token buckets.

### SAD-COMP-003 — Asynchronous Processing Framework (Asynq)
**Delivers:** FR-FUP-002..003, FR-BIL-004, FR-NOTIF-001..011, FR-REV-002
**Detail:** [MDS §4.4](04_MDS_Module_Design.md)

75-worker Asynq pool on Redis. Handles: bulk PostgreSQL COPY flushes (every 300s), CoA/PoD delivery with retry, dunning stage execution, notification dispatch (WhatsApp + SMS + email), PII re-encryption rotation, invoice PDF generation, revenue reconciliation jobs.

### SAD-COMP-004 — Relational Storage Core (PostgreSQL 15)
**Delivers:** FR-BIL-001..007, FR-REV-001..004, NFR-DUR-001
**Detail:** [DBD](06_DBD_Database_Design.md)

Primary + synchronous streaming read replica. Absolute authority for financial records, subscriber profiles, notification logs, CGNAT allocations, franchise ledgers, and audit trails. Analytics and dashboard queries route to replica.

### SAD-COMP-005 — Notification Service (WhatsApp Business API + SMS + Email)
**Delivers:** FR-NOTIF-001..011, CRD-NOTIF-001..002, CRD-REG-003
**Detail:** [MDS §4.7](04_MDS_Module_Design.md), [DDS §5.8](05_DDS_Detailed_Design.md)

Dedicated notification dispatcher invoked by Asynq tasks. Responsibilities:
- DND flag check before every outbound message
- WhatsApp Business API template dispatch with delivery status callback ingestion
- SMS gateway dispatch (configurable provider: Twilio / MSG91 / Exotel)
- Email dispatch via SMTP
- `notification_log` record creation per message (FR-NOTIF-009)
- WhatsApp delivery status webhook receiver: `sent → delivered → read / failed` (FR-NOTIF-011)

### SAD-COMP-006 — API Gateway & RBAC
**Delivers:** FR-SEC-005, FR-OBS-004, FR-REV-003..004, FR-SUB-001..005, FR-FRN-003
**Detail:** [DDS §5.7](05_DDS_Detailed_Design.md), [API](07_API_OpenAPI_Contract.md)

HTTP API layer with JWT middleware. Four roles + franchise role. Exposes subscriber health endpoint (SAD-COMP-008), revenue dashboard, collections forecast, and franchise P&L view.

### SAD-COMP-007 — Observability Stack
**Delivers:** FR-OBS-001..005, NFR-AVAIL-001, CRD PER-002
**Detail:** [IDD §8.6](08_IDD_Infrastructure_Design.md)

- Prometheus metrics on all services; Grafana dashboards
- Structured JSON logs via zerolog → Loki
- PagerDuty alerting for: uptime failures, dead-letter queue depth > 0, Redis failover, PostgreSQL replication lag > 5s, **NAS auth failure rate > 20% over 5 min (FR-OBS-005)**
- Correlation IDs propagated through all service calls for cross-service tracing

### SAD-COMP-008 — Subscriber Health API *(new — gap PER-002, PER-004)*
**Delivers:** FR-OBS-004, CRD PER-002, PER-004, PER-005
**Detail:** [DDS §5.9](05_DDS_Detailed_Design.md), [API §7](07_API_OpenAPI_Contract.md)

Single-call `GET /api/v1/subscribers/{id}/health` that aggregates: active session state (from Redis), FUP status, current speed profile, last CoA result, wallet balance, plan expiry, open ticket count, last notification sent (from `notification_log`). Designed for CSR to answer a complaint call in under 30 seconds.

### SAD-COMP-009 — Revenue Assurance Module *(new — gap BO-001)*
**Delivers:** FR-REV-001..004, CRD-REV-001..002, CRD BO-001
**Detail:** [MDS §4.8](04_MDS_Module_Design.md)

Scheduled Asynq job (nightly) and on-demand API that: identifies unbilled active subscribers, reconciles ledger totals, computes 30-day forward collections forecast, and feeds the collections dashboard.

### SAD-COMP-010 — Subscriber Self-Service Portal *(new — gap PER-006)*
**Delivers:** FR-SUB-001..005, CRD PER-006, CRD BO-003
**Detail:** [MDS §4.9](04_MDS_Module_Design.md)

Web-based portal (responsive HTML/JS). Authenticated via subscriber-specific JWT. Shows real-time usage, plan details, wallet, invoices, notification history, and ticket management. Renewal deep-links to Razorpay/BBPS payment flow.

### SAD-COMP-011 — Franchise / LCO Module *(new — gap BO-004)*
**Delivers:** FR-FRN-001..003, CRD-FRN-001, CRD BO-004
**Detail:** [MDS §4.10](04_MDS_Module_Design.md)

Multi-tenant data isolation via `franchise_id` column on subscriber and ledger tables. LCO portal is a scoped view of the main API. Commission calculation runs as an Asynq job on each recharge event.

---

## 3.3 Data Flow & Concurrency Management

### RADIUS Authentication Hot Path
```
NAS (CCR2004) → UDP :1812 → Master Listener
  → Channel → Worker Pool (128 goroutines)
    → Redis HGET subscriber:{username}
      → [HIT]  → Build RADIUS Accept; respond in <5ms
      → [MISS] → Fallback PostgreSQL; cache; respond
```

### Accounting & FUP Path
```
NAS → UDP :1813 → Worker
  → Redis SetNX dedup check (30s TTL)
    → [DUPLICATE] → ACK; skip
    → [NEW]
        → Redis HINCRBY usage:{session_id}
        → XADD accounting_stream
        → FUP goroutine: compare usage vs threshold
          → [80% reached]  → Asynq enqueue NOTIF task → WhatsApp + SMS to subscriber
          → [100% breach]  → Asynq enqueue CoA task + NOTIF task (throttle notification)
              → [ACK received] → complete
              → [Timeout × 5] → dead-letter; PagerDuty alert
```

### Notification Dispatch Path
```
Asynq NOTIF task
  → Check dnd_opt_out flag (PostgreSQL)
    → [opted out]  → skip dispatch; log suppression in notification_log
    → [allowed]
        → WhatsApp Business API (template dispatch)
        → SMS gateway
        → Email (if dunning reminder)
        → Write notification_log record (FR-NOTIF-009)
  → WhatsApp delivery webhook callback
        → Update notification_log.delivery_status (sent → delivered → read / failed)
```

### Bulk PostgreSQL Flush (every 300s)
```
Asynq flush worker → XREAD accounting_stream (batched)
  → PostgreSQL COPY subscriber_session_history
  → XACK stream entries
```

---

## 3.4 High Availability & Disaster Recovery

| Asset | Method | Frequency | RTO | RPO | Ops Ref |
|---|---|---|---|---|---|
| PostgreSQL data | WAL archiving + pg_dump | WAL: 5 min / Full: weekly | 5 min (failover) / 2h (restore) | 0 (sync replica) | OPS §12.3.2 |
| Redis data | AOF + RDB | AOF: continuous / RDB: 15 min | 3s (Sentinel) | ≤15 min | OPS §12.3.1 |
| Configuration | Git | On every change | 10 min | Indefinite | — |
| TLS certificates | Auto-renew | 30d before expiry | 10 min | — | — |

Redis Sentinel: 3-node quorum (2 of 3 must agree for failover). Automated master promotion ≤ 8 seconds (retargeted from 3s — measured 3086-3192ms at the lowest safe detection setting, see TST §13.4). AAA daemon reconnects within one retry cycle (≤ 500ms) of promotion completing.
