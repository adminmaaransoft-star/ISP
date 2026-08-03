# Document 1: Customer Requirements Document (CRD)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
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
- NAS/OLT SNMP monitoring (flagged as a future phase; see § 1.7)

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

## 1.10 Future Phase (Out-of-Scope v1, Planned v2)

| Item | Rationale |
|---|---|
| NAS/OLT SNMP health monitoring | Addresses NOC proactive alerting gap (PER-002); deferred to v2 |
| Android / iOS subscriber mobile app | Addresses subscriber self-service gap (PER-006); web portal in v1 |
| AI-based churn prediction | Addresses BO-005 analytics depth; deferred to v2 |
| Postpaid billing model | Prepaid-only in v1 |
