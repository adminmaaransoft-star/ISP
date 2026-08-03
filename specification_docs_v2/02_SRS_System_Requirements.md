# Document 2: System Requirements Specification (SRS)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** SRS
**Traces From:** [CRD](01_CRD_Customer_Requirements.md)
**Traces To:** [SAD](03_SAD_System_Architecture.md) → [MDS](04_MDS_Module_Design.md) → [DDS](05_DDS_Detailed_Design.md) → [DBD](06_DBD_Database_Design.md) → [API](07_API_OpenAPI_Contract.md) → [TST](13_TST_Test_Strategy.md)

---

## 2.1 Functional Requirements Matrix

### AAA / Authentication

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-AAA-001 | Process concurrent UDP RADIUS requests on port 1812 (Auth) and 1813 (Accounting) | — | MDS §4.1 |
| FR-AAA-002 | Authenticate users against Redis cache in ≤ 5 ms | — | MDS §4.1 |
| FR-AAA-003 | Deduplicate Interim-Update packets via atomic Redis SetNX (session ID + octet count key) | — | DDS §5.2 |
| FR-AAA-004 | Write subscriber session to Redis on first auth; TTL = plan validity period | — | MDS §4.1 |

### Billing & Finance

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-BIL-001 | Calculate GST: 9% CGST + 9% SGST if `registered_state == 'TN'`; 18% IGST otherwise | — | MDS §4.3 |
| FR-BIL-002 | All tax computations must use `decimal.Decimal` with banker's rounding | — | DDS §5.4 |
| FR-BIL-003 | Wallet recharge must execute as atomic double-entry ledger transaction | — | DDS §5.6 |
| FR-BIL-004 | Dunning engine must fire notifications at T-7d, T-3d, T-1d; CoA throttle at T+24h; PoD at T+72h | CRD §1.7 | MDS §4.3 |
| FR-BIL-005 | Wallet recharge endpoint must enforce `transaction_token` idempotency | CRD-PAY-001 | DDS §5.6 |
| FR-BIL-006 | System must generate GSTR-1 compatible export: HSN summary, B2B/B2C split, state-wise breakdown | CRD §1.3 | MDS §4.3 |
| FR-BIL-007 | Invoices must include plain-language usage summary (GB used / GB included) for subscriber clarity | CRD PER-006 | MDS §4.3 |

### FUP & Session Management

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-FUP-001 | Continuously evaluate Redis traffic counters vs plan bounds; trigger CoA on breach | — | MDS §4.2 |
| FR-FUP-002 | CoA and PoD must retry via Asynq with exponential backoff, max 5 attempts | — | DDS §5.3 |
| FR-FUP-003 | Failed CoA/PoD tasks exhausting retries → dead-letter queue + operator alert ≤ 60s | — | MDS §4.2 |
| FR-FUP-004 | System must send WhatsApp + SMS notification when subscriber reaches 80% of FUP threshold | CRD-NOTIF-001, CRD §1.7 | MDS §4.7 |
| FR-FUP-005 | System must send WhatsApp + SMS notification when FUP throttle is applied (with reason and restore instructions) | CRD-NOTIF-001 | MDS §4.7 |

### Security

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-SEC-001 | Block identity after 10 invalid credentials within 60 s (Redis token bucket; 15-min ban) | — | MDS §4.1 |
| FR-SEC-002 | All PII fields (Aadhaar, PAN) must be AES-GCM-256 encrypted before any DB write | CRD-REG-002 | DDS §5.5 |
| FR-SEC-003 | Encrypted PII must store key version ID in ciphertext for cross-rotation decryption | CRD-REG-002 | DDS §5.5 |
| FR-SEC-004 | All inbound payment webhooks must be HMAC-SHA256 validated before state mutation | CRD-PAY-001 | DDS §5.6 |
| FR-SEC-005 | All admin API routes must require valid JWT bearer token; role enforced per route | — | DDS §5.7 |

### Notifications — WhatsApp, SMS, Email

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-NOTIF-001 | Send dunning reminders via WhatsApp + SMS + email at T-7d, T-3d, T-1d | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-002 | Send WhatsApp + SMS when subscriber reaches 80% FUP threshold | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-003 | Send WhatsApp + SMS when FUP throttle is applied (speed reduced) | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-004 | Send WhatsApp + SMS payment receipt on successful wallet recharge | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-005 | Send WhatsApp + SMS on soft suspension (T+24h), stating reason and payment link | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-006 | Send WhatsApp + SMS on service restoration after recharge | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-007 | Send WhatsApp notification on ticket status change | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-008 | All notifications must validate `dnd_opt_out` flag before dispatch | CRD-REG-003 | MDS §4.7 |
| FR-NOTIF-009 | Every outbound notification must create a `notification_log` record with: channel, template ID, subscriber ID, event, timestamp, delivery status, failure reason | CRD-NOTIF-002 | DBD §6.2 |
| FR-NOTIF-010 | WhatsApp messages must use pre-approved Business API templates; template ID must be stored in notification config | CRD-NOTIF-001 | MDS §4.7 |
| FR-NOTIF-011 | System must store WhatsApp delivery status callbacks: sent → delivered → read / failed | CRD-NOTIF-001 | DBD §6.2 |

### Observability

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-OBS-001 | Expose Prometheus `/metrics`: RADIUS auth latency histograms, CoA ACK rates, active sessions, FUP breach counters | — | MDS §4.1 |
| FR-OBS-002 | All logs emitted as structured JSON: `timestamp`, `level`, `service`, `correlation_id`, `message`, `subscriber_id` | — | SAD §3.2 |
| FR-OBS-003 | All LEA data access events must write tamper-evident audit record | CRD-REG-001 | DBD §6.2 |
| FR-OBS-004 | Expose subscriber health endpoint (single-call diagnostic view) for CSR and NOC use | CRD PER-002, PER-004 | API §7 |
| FR-OBS-005 | System must emit proactive alert when RADIUS auth failure rate on any NAS exceeds 20% over 5 min | CRD PER-002 | SAD §3.2 |

### CGNAT & Network

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-NET-001 | Record CGNAT port-block allocations: public IP, port range, subscriber ID, timestamps | CRD-REG-001 | DBD §6.2 |
| FR-NET-002 | Expose secured LEA lookup API: public IP + port + timestamp → subscriber identity | CRD-REG-001 | API §7 |
| FR-NET-003 | Record IPv6 prefix delegations in `subscriber_session_history` | — | DBD §6.2 |

### Revenue Assurance *(new — gap BO-001)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-REV-001 | System must produce unbilled-subscriber report: all active subscribers with no invoice in current cycle | CRD-REV-001 | MDS §4.8 |
| FR-REV-002 | System must reconcile sum(wallet_balance) against sum(ledger credits) and flag variance > ₹0.01 | CRD-REV-001 | MDS §4.8 |
| FR-REV-003 | Collections dashboard must show outstanding balance by dunning stage and month-over-month recovery rate | CRD-REV-002 | API §7 |
| FR-REV-004 | System must generate 30-day forward collections forecast based on expiry dates and wallet balances | CRD-REV-002 | MDS §4.8 |

### Subscriber Self-Service Portal *(new — gap PER-006)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-SUB-001 | Subscriber portal must display real-time data usage (current session GB used / plan GB total) | CRD PER-006 | MDS §4.9 |
| FR-SUB-002 | Portal must display current plan, expiry date, wallet balance, and last 3 invoices | CRD PER-006 | MDS §4.9 |
| FR-SUB-003 | Portal must allow one-tap plan renewal via Razorpay / BBPS payment link | CRD PER-006 | MDS §4.9 |
| FR-SUB-004 | Portal must allow subscriber to raise a support ticket and view its status | CRD PER-006 | MDS §4.9 |
| FR-SUB-005 | Portal must display notification delivery history (what was sent, when, channel) | CRD-NOTIF-002 | MDS §4.9 |

### Franchise / LCO *(new — gap BO-004)*

| FR ID | Requirement | CRD Ref | Module |
|---|---|---|---|
| FR-FRN-001 | System must support multi-tenant LCO accounts: each LCO manages their own subscribers under a parent ISP | CRD-FRN-001 | MDS §4.10 |
| FR-FRN-002 | LCO commission must be calculated per recharge and tracked in a separate LCO ledger | CRD-FRN-001 | MDS §4.10 |
| FR-FRN-003 | Parent ISP must see consolidated P&L across all LCO partners via a franchise analytics view | CRD-FRN-001 | API §7 |

---

## 2.2 Non-Functional Requirements Matrix

| NFR ID | Category | Requirement | Validation Method | CRD Ref | TST Ref |
|---|---|---|---|---|---|
| NFR-PERF-001 | Latency | RADIUS auth round-trip ≤ 15 ms at peak load | `radperf` at 5,000 req/s for 10 min | — | TST §13.4 |
| NFR-PERF-002 | Latency | API p99 ≤ 200 ms at 500 concurrent users | k6 load test | — | TST §13.4 |
| NFR-PERF-003 | Latency | WhatsApp/SMS notification dispatch ≤ 5 s from triggering event | End-to-end timing in integration test | CRD-NOTIF-001 | TST §13.3 |
| NFR-SCAL-001 | Concurrency | 20,000 active concurrent PPPoE tunnels without starvation | Load test: ramp 30 min, hold 1h | CRD BO-006 | TST §13.4 |
| NFR-AVAIL-001 | Availability | Core control plane ≥ 99.99% uptime (rolling 12 months) | Synthetic probe every 30 s | — | TST §13.4 |
| NFR-DUR-001 | Durability | Accounting counts: zero data loss on single-node failure | Chaos test: Redis kill during storm | — | TST §13.4 |
| NFR-SEC-001 | Security | TLS 1.3 minimum on all external endpoints | `testssl.sh` scan | CRD-REG-002 | — |
| NFR-SEC-002 | Security | No plaintext PII in logs, DB, or error responses | Automated PII scanner in CI | CRD-REG-002 | TST §13.5 |
| NFR-BIZ-001 | Revenue | Unbilled-subscriber report must run within 60 s for 20,000 subscribers | Timed query test | CRD-REV-001 | TST §13.3 |

---

## 2.3 Hardware & Software Dependencies

| Component | Minimum Version | Notes |
|---|---|---|
| Linux Kernel | 5.4 | Ubuntu 22.04 LTS or Debian 12 |
| Go | 1.21 | AAA daemon, API service, migration tooling |
| PostgreSQL | 15 | Primary + synchronous read replica |
| Redis | 7.2 | Sentinel HA cluster (3 nodes) |
| Gotenberg | 8.0 | Invoice PDF generation |
| WhatsApp Business API | Cloud API v17+ | Meta Business Account required; pre-approved templates |
| SMS Gateway | — | Provider configurable (e.g., Twilio, Exotel, MSG91) |
| Docker Engine | 24.0 | |
| Docker Compose | 2.0 | |

---

## 2.4 Requirements Traceability Summary

| FR Group | Count | Primary Doc Owner | Test Coverage |
|---|---|---|---|
| AAA | 4 | DDS §5.1–5.2 | TST INT-AAA-001..005 |
| Billing & Finance | 7 | DDS §5.4–5.6, MDS §4.3 | TST INT-BIL-001..006 |
| FUP & Session | 5 | DDS §5.3, MDS §4.2 | TST INT-FUP-001..004 |
| Security | 5 | DDS §5.5–5.7 | TST INT-SEC-001..004 |
| Notifications (WhatsApp/SMS/Email) | 11 | MDS §4.7 | TST INT-NOTIF-001..008 |
| Observability | 5 | SAD §3.2, MDS §4.1 | TST INT-OBS-001..003 |
| CGNAT / Network | 3 | DBD §6.2 | TST INT-NET-001..002 |
| Revenue Assurance | 4 | MDS §4.8 | TST INT-REV-001..004 |
| Subscriber Portal | 5 | MDS §4.9 | TST INT-SUB-001..004 |
| Franchise / LCO | 3 | MDS §4.10 | TST INT-FRN-001..002 |
| **Total** | **52** | | |
