# Document 1: Customer Requirements Document (CRD)
**Version:** 3.0 | **Status:** Draft | **Date:** 2026-08-12
**Document ID:** CRD
**Traceability:** This document is the root requirement source. All IDs defined here are traced forward into → [SRS](02_SRS_System_Requirements.md) → [SAD](03_SAD_System_Architecture.md) → [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md) → [DBD](06_DBD_Database_Design.md) → [API](07_API_OpenAPI_Contract.md)

---

## 1.1 Product Vision & Business Objectives

The objective is to deploy a high-availability, high-performance, unified BSS/OSS platform to manage retail and corporate FTTH and IPoE/PPPoE operations for Indian ISPs. The platform replaces commercial middleware (e.g., Jaze ISP Manager), eliminating database transaction locks during authentication storms on MikroTik CCR2004 hardware.

### Owner-Level Business Outcomes (What the Owner Buys)

| Outcome ID | Business Outcome | Delivery Mechanism | Traced To |
|---|---|---|---|
| BO-001 | Revenue leakage ≤ 0.5% of annual billing (industry avg is 1–3%) | Activation-coupled billing, dedup accounting, reconciliation reports | FR-REV-001..004 |
| BO-002 | Zero DoT/DPDP licence-risk exposure | PII encryption, LEA audit API, CAF/KYC archival | FR-SEC-002..003, FR-NET-002 |
| BO-003 | Staff cost reduction: 2+ FTE tasks automated | Dunning automation, self-renewal portal, CoA auto-throttle | FR-BIL-004, FR-SUB-001..003 |
| BO-004 | Ability to onboard LCO/franchise partners without new systems | Multi-tenant franchise billing module | FR-FRN-001..003 |
| BO-005 | ARPU visibility and churn early-warning | Collections dashboard, dunning funnel, plan analytics | FR-REV-003..004 |
| BO-006 | Scale from 500 to 20,000 subscribers on same infrastructure | Worker pool architecture, Redis HA, partitioned DB | NFR-SCAL-001 |

---

## 1.2 User Personas & Stakeholder Matrix

| Persona ID | Persona | What They Actually Buy | Core Feature Needs |
|---|---|---|---|
| PER-001 | **ISP Owner** | Margin protection, compliance indemnity, growth capacity | Revenue reconciliation, franchise module, analytics dashboard |
| PER-002 | **NOC Engineer** | Fewer 3am calls, fast diagnosis, proactive alerts | Single-screen subscriber health view, NAS alert integration, pre-dispatch diagnostics |
| PER-003 | **Billing / Finance Admin** | Clean month-end close, zero GST filing errors, cash flow visibility | GSTR-1 export, unbilled subscriber report, collections forecast, dunning funnel |
| PER-004 | **CSR** | Not being embarrassed in front of a subscriber | Notification delivery log, one-call resolution view, subscriber health API |
| PER-005 | **Ground Technician** | Not driving to a site unnecessarily | Pre-dispatch diagnostic summary, closed-loop dispatch confirmation |
| PER-006 | **End Subscriber** | Trust: no surprise disconnections, transparent billing | Real-time usage dashboard, 80% data warning, plain-language invoice, one-tap renewal |

---

## 1.3 Scope of Work

### In-Scope

- Concurrent RADIUS AAA server core (Go-based worker pools)
- Write-through session management engine (Redis 7.x Sentinel HA)
- GST-compliant billing, double-entry wallet, GSTR-1 export
- Automated mid-session FUP throttling via CoA (with subscriber WhatsApp notification on breach)
- Application-layer PII encryption (AES-GCM-256, 90-day key rotation)
- CGNAT port-block allocation tracking and LEA export API
- Webhook-based payment integration (Razorpay, BBPS) with HMAC validation
- RBAC for NOC, billing, CSR, technician roles
- Structured observability: Prometheus metrics, structured logging, PagerDuty alerting
- **Revenue assurance module: unbilled subscriber report, reconciliation, leakage dashboard** *(gap BO-001)*
- **Subscriber self-service portal: real-time usage, renewal, tickets** *(gap BO-003, PER-006)*
- **WhatsApp Business API notification channel** (dunning, FUP alerts, payment receipts) *(gap identified)*
- **Franchise / LCO multi-tenant billing module** *(gap BO-004)*
- **Collections dashboard and dunning funnel analytics** *(gap BO-005)*
- **GSTR-1 compatible GST export** *(gap PER-003)*
- **Notification delivery log per subscriber** *(gap PER-004)*
- **Subscriber health API (single-call diagnostic view for CSR and technician)** *(gap PER-002, PER-004)*

### Out-of-Scope

- Native Layer-2 switching control plane (handled off-box by OLTs and MikroTik hardware)
- Direct NetFlow stream processing (off-box via `pmacct`)
- Native payment gateway hosting
- IPv6 prefix delegation control plane (tracked in session history only)
- NAS/OLT SNMP monitoring

> **v3 note:** Multi-vendor NAS attribute support, mobile app, franchise-module
> activation, and CPE auto-provisioning were previously deferred as "future
> phase" items below. As of v3 they are in-scope, phased — see **§ 1.11**.
> They are called out separately from the list above because they are
> scoped and staged for delivery, not merely acknowledged and shelved.

---

## 1.4 Regulatory Compliance Requirements

### CRD-REG-001 — DoT / TRAI Compliance

- The system must record CAF numbers, KYC status, and session logs mapping IP address distributions to the millisecond.
- LEA IP-to-subscriber lookup must be available via a secured, audited API. All access events must be tamper-evidently logged.
- CGNAT port-block records must be retained per DoT guidelines.

### CRD-REG-002 — DPDP Act Compliance

- All PII (Aadhaar, PAN) must be AES-GCM-256 encrypted at application layer with 90-day key rotation.
- Ciphertext must embed key version ID. Rotation job must be idempotent and resumable.
- Plaintext PII storage is prohibited in database, logs, and error responses.

### CRD-REG-003 — TRAI DND Integration

- All outbound notifications (SMS, WhatsApp, email) must validate `dnd_opt_out` before dispatch.
- Dunning and marketing notifications must not be sent over transactional paths to opted-out subscribers.
- WhatsApp Business API messages are classified as transactional when triggered by billing events; DND check still required.

---

## 1.5 Payment Gateway Security Requirements

### CRD-PAY-001

- All inbound Razorpay and BBPS webhooks must be validated against HMAC-SHA256 signatures before any state change.
- Wallet recharge endpoints must enforce idempotency via `transaction_token`.
- Webhook delivery failures must be logged for manual reconciliation.

---

## 1.6 Notification Channel Requirements

### CRD-NOTIF-001 — WhatsApp Business API

The system must support WhatsApp as a first-class notification channel alongside SMS and email.

| Use Case | Template Type | DND Check | Traced To |
|---|---|---|---|
| Renewal reminder (T-7d, T-3d, T-1d) | Transactional | Required | FR-NOTIF-001 |
| FUP threshold reached (80% warning) | Transactional | Required | FR-NOTIF-002 |
| FUP throttle applied | Transactional | Required | FR-NOTIF-003 |
| Payment received confirmation | Transactional | Not required | FR-NOTIF-004 |
| Suspension warning | Transactional | Required | FR-NOTIF-005 |
| Service restored after recharge | Transactional | Not required | FR-NOTIF-006 |
| Ticket status update | Transactional | Not required | FR-NOTIF-007 |

WhatsApp messages must use pre-approved Business API templates. The system must store delivery status (sent, delivered, read, failed) per message per subscriber in `notification_log`.

### CRD-NOTIF-002 — Notification Delivery Log

Every outbound notification (SMS, WhatsApp, email) must create a `notification_log` record containing: channel, template ID, subscriber ID, triggered-by event, sent timestamp, delivery status, and failure reason if applicable. CSR must be able to retrieve this log per subscriber to resolve billing disputes.

---

## 1.7 Dunning Policy Requirements

| Stage | Trigger | Channels | DND Check | Traced To |
|---|---|---|---|---|
| Data 80% warning | 80% of FUP threshold reached | WhatsApp + SMS | Required | FR-NOTIF-002 |
| Renewal reminder 1 | T-7d before expiry | WhatsApp + SMS + email | Required | FR-BIL-004 |
| Renewal reminder 2 | T-3d before expiry | WhatsApp + SMS | Required | FR-BIL-004 |
| Renewal reminder 3 | T-1d before expiry | WhatsApp + SMS | Required | FR-BIL-004 |
| Grace period | Expiry day | — | — | FR-BIL-004 |
| Soft suspension | T+24h | WhatsApp + SMS (reason stated) | Required | FR-BIL-004 |
| Hard suspension | T+72h | WhatsApp + SMS | Required | FR-BIL-004 |
| Reactivation | Wallet recharged | WhatsApp + SMS (confirmation) | Not required | FR-BIL-004 |

> Grace period is configurable per plan tier. Corporate accounts default to 72h.

---

## 1.8 Revenue Assurance Requirements *(new — gap BO-001)*

### CRD-REV-001

The system must provide a revenue reconciliation report showing:
- All active subscribers with no invoice raised in the current billing cycle (unbilled subscriber list)
- Sum of wallet balances vs sum of ledger entries (variance must be zero)
- Sessions active with no corresponding billing record

### CRD-REV-002

The system must provide a collections dashboard showing:
- Outstanding balance by dunning stage (grace, soft-suspended, hard-suspended)
- Month-over-month dunning recovery rate
- 30-day forward collections forecast based on expiry dates and wallet balances

---

## 1.9 Franchise / LCO Requirements *(new — gap BO-004)*

### CRD-FRN-001

The system must support a multi-tenant franchise model where:
- Each LCO partner manages their own subscriber roster under a parent ISP account
- LCO wallets are maintained separately; commission is tracked per recharge
- The parent ISP sees a consolidated P&L across all LCO partners
- LCO partners access a restricted portal; they cannot see other LCOs' subscribers

---

## 1.10 Future Phase (Out-of-Scope v2, Planned v3+)

| Item | Rationale |
|---|---|
| NAS/OLT SNMP health monitoring | Addresses NOC proactive alerting gap (PER-002); superseded by § 1.11 Phase 2 (NAS attribute work covers the CoA/PoD side; SNMP polling itself remains deferred beyond v3) |
| AI-based churn prediction | Addresses BO-005 analytics depth; still deferred beyond v3 |
| Postpaid billing model | Prepaid-only through v3 |

> Mobile app and franchise-module activation, previously listed here, moved
> to § 1.11 (in-scope, phased) once BO-007 was adopted.

---

## 1.11 Jaze-Parity Expansion Requirements *(new — gap BO-007, v3)*

### Why this section exists

§ 1.1 states the product replaces commercial middleware such as Jaze ISP
Manager. Through v2, "replaces" meant the AAA/billing/notification core —
genuinely deep and well-verified, but narrower than what an ISP owner
evaluating Jaze actually compares against: a full operations suite covering
CRM, inventory, franchise administration, task workflows, and multi-vendor
network support. A 2026-08-12 gap analysis against a reference Jaze
architecture found the core sound but identified the following as absent or
present-but-unreachable (see `internal/revenue/franchise.go`, built but never
routed — the sharpest example). This section adopts the full breadth as a
phased, in-scope roadmap rather than a parity checklist with no sequencing.

### BO-007 — Full ISP operations-suite parity

| Outcome ID | Business Outcome | Delivery Mechanism | Traced To |
|---|---|---|---|
| BO-007 | Owner can run the business — sales pipeline through field ops — on one platform, matching or exceeding commercial ISP-manager suites | Phases 2–5 below | FR groups: NAS, HSP, API, MOB, SUP, CRM, INV, WFL, ANN, FRN (extended), RPT, DOC, CPE; NFR-AVAIL-002, NFR-TEN-001 |

### Phasing rationale

Ordered by what breaks the product versus what extends it. Phase 2 fixes
things that are silently wrong today (a non-MikroTik router gets no working
speed enforcement; a single Postgres instance is a single point of failure
for the money). Phase 3 closes the operations-suite gap — the modules an
owner evaluating this against Jaze would notice missing first. Phase 4 adds
channel and integration breadth. Phase 5 is the hosting-model change
(single-tenant on-prem → multi-tenant SaaS), which is an architectural
decision, not an incremental feature, and is sequenced last deliberately.

### CRD-EXP-001 — Phase 2: Network-layer correctness (do first)

The current build enforces bandwidth policy correctly only on MikroTik NAS
devices — the RADIUS transport is vendor-neutral but the rate-limit
attribute sent in every Access-Accept and CoA packet is a hardcoded MikroTik
VSA. Any Cisco, Juniper, Huawei, ZTE, or wireless-controller NAS in the
network today authenticates subscribers but silently does not throttle them
to their plan speed. Phase 2 closes this and the matching single-point-of-
failure gap on PostgreSQL (Redis already has real Sentinel HA; the database
holding the money does not).

- Per-vendor RADIUS attribute construction (Cisco `cisco-avpair`, Juniper,
  Huawei vendor-2011, MikroTik vendor-14988, generic RFC 3576)
- A `nas_devices` table so each NAS can have its own secret, vendor, and
  CoA/PoD port, rather than one global shared secret for every router
- CHAP support for NAS devices that require challenge-response auth (PAP-only
  today, a direct consequence of storing only a bcrypt hash — see FR-AAA-005)
- CoA fires on plan change/top-up, not just FUP breach and manual override
  (today a mid-cycle upgrade doesn't reach an already-connected session)
- PostgreSQL streaming replica with automated failover

### CRD-EXP-002 — Phase 3: Operations-suite parity

The modules an owner comparing this platform to a commercial ISP-manager
suite would look for first, beyond billing and AAA.

- **Helpdesk with SLA** — today's tickets have no priority or due-by time;
  add both plus breach alerting
- **CRM / lead pipeline** — track a prospect before they're a subscriber,
  through to conversion, with funnel reporting
- **Inventory / CPE tracking** — device serials, vendor, warehouse location,
  issue/return status, linked to the subscriber at onboarding
- **Task & approval workflows** — second-approver sign-off for sensitive
  actions (large wallet credits, terminations), plus ad hoc field-task
  assignment independent of the ticket system
- **Announcements** — staff-composed broadcast (maintenance notices, outage
  alerts) to all subscribers or a filtered segment, reusing the existing
  DND/notification-log machinery
- **Franchise module, actually reachable** — the commission/P&L engine exists
  in code (`internal/revenue/franchise.go`) but has no route and no console
  screen; wire it up, add restricted LCO-partner portal login, and a staff
  onboarding flow for new franchise accounts (this was CRD-FRN-001's original
  ask — it was scoped once already and never finished)
- **General reporting** — subscriber growth/churn, plan-mix distribution,
  ticket-resolution metrics, and per-area collection performance, not just
  revenue reconciliation

### CRD-EXP-003 — Phase 4: Channel and integration breadth

- Email as a notification channel, alongside WhatsApp and SMS
- Push notifications (OneSignal or FCM/APNs) for a mobile app
- A mobile-facing API with the same self-service capabilities as the web
  portal (usage, invoices, renewal, tickets, notification history)
- Captive portal + MAC-auth for hotspot NAS devices (today only PPPoE-style
  username/password auth exists)
- Partner-facing API-key authentication, distinct from internal staff JWTs,
  plus outbound webhooks so a partner can subscribe to subscriber lifecycle
  events instead of polling
- Document archival to external storage (S3-compatible, Google Drive, or
  SFTP) for invoices/KYC, not just local Gotenberg rendering
- TR-069/CWMP auto-provisioning so a new CPE gets its PPPoE credentials and
  bandwidth profile without a technician configuring it by hand

### CRD-EXP-004 — Phase 5: Multi-tenant SaaS hosting

Everything through Phase 4 still assumes one Postgres + one Redis cluster per
ISP operator (franchise/LCO multi-tenancy is row-level within that one
deployment). A hosted/managed offering — one instance of this platform
serving multiple *unrelated* ISP operators — is a distinct hosting-model
decision: schema-per-tenant vs. database-per-tenant, tenant-aware routing,
and billing-for-the-platform-itself are new architectural surface, not an
extension of existing tables. Scoped last on purpose — it should follow
Phase 2–4 landing on real deployments, not precede it.
