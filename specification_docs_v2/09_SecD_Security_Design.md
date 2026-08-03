# Document 9: Security Design & Threat Model (SecD)
**Version:** 2.0 | **Status:** Draft | **Date:** 2025-06-01
**Document ID:** SecD
**Traces From:** [SRS](02_SRS_System_Requirements.md) → [MDS](04_MDS_Module_Design.md)
**Traces To:** [DDS](05_DDS_Detailed_Design.md) → [TST](13_TST_Test_Strategy.md)

---

## 9.1 STRIDE Threat Analysis

| Threat | Vector | Mitigation |
|---|---|---|
| **Spoofing** | Malicious actor replicates subscriber router profile | Enforce CHAP authentication on PPPoE; reject PAP profiles at NAS level |
| **Tampering** | Internal user modifies billing transaction lines | DB constraint `chk_gst_logic` on invoices; Gotenberg PDFs signed with hash; LEA audit log append-only via row security policy |
| **Repudiation** | Admin denies issuing a manual CoA or PoD | Structured audit log (correlation ID, JWT identity, action, target, timestamp) on all state-modifying API calls |
| **Information Disclosure** | PAN or Aadhaar exposed in logs or error responses | AES-GCM-256 application-layer encryption; PII scanner in CI; structured error responses never echo raw field values |
| **Denial of Service** | Authentication storm from compromised CPE | Redis token bucket brute-force guard: 10 failures / 60s = 15-minute MAC block; RADIUS worker pool caps goroutine count |
| **Elevation of Privilege** | CSR accesses billing admin routes | JWT role claim validated at route middleware; roles are non-mutable by callers |

---

## 9.2 Data-at-Rest & Data-in-Transit Encryption

### In-Transit

- All external API traffic, webhook calls, and customer portal connections route through TLS 1.3 (minimum). TLS 1.2 and below are disabled at the reverse proxy.
- Internal RADIUS shared secrets: minimum 32-character random strings, rotated annually.
- Internal service-to-service calls within Docker network use private subnets (no TLS required on LAN; access blocked at network boundary).

### At-Rest

- PostgreSQL volumes encrypted at the OS layer via LUKS (or cloud provider volume encryption).
- PII fields (Aadhaar, PAN) additionally encrypted at the application layer with AES-GCM-256 (defense in depth).
- Redis AOF/RDB files on encrypted volumes. Redis does not receive raw PII — only ciphertext is written to Redis if PII must be cached.

---

## 9.3 AES Key Management & Rotation

### Key Storage

- Key material stored exclusively in a secret manager (HashiCorp Vault or AWS Secrets Manager). Keys are never stored in the database, environment variables, or application config files.
- The database stores only the `version_id` (e.g., `v1`, `v2`) and a SHA-256 hash of the key material for audit purposes.

### Key Versioning & Rotation Protocol

Every ciphertext produced by the system uses the format:

```
{version_id}:{base64(nonce + ciphertext)}
```

Example: `v3:Zm9vYmFy...`

This allows the decryption service to fetch the correct key by version ID regardless of the current active key.

**Rotation procedure (every 90 days):**

1. Generate new key material; store in secret manager as `v{N+1}`.
2. Insert new `encryption_keys` row with `status = 'active'`; update previous key to `status = 'retired'`.
3. Trigger the `pii_rotation` Asynq job.
4. Job processes `kyc_verifications` records in batches of 500, in a DB transaction:
   - Fetch record; decrypt using `key_version_id` from ciphertext prefix.
   - Re-encrypt with new key version.
   - Update `aadhaar_encrypted`, `pan_encrypted`, and `key_version_id`.
   - Commit batch.
5. Job is idempotent: records already at the latest `version_id` are skipped.
6. Job is resumable: failure mid-run resumes from the last committed batch on next execution.
7. On completion, update `encryption_keys.rotated_at` for the retired key.

> **Critical:** The rotation job must never delete or overwrite the retired key from the secret manager until all records have been confirmed re-encrypted. Retired keys must be retained for at least 90 days post-rotation for decryption of any records missed during the rotation window.

---

## 9.4 JWT Authentication & Authorization

### Token Structure

```json
{
  "sub": "admin_user_42",
  "role": "billing_admin",
  "iss": "bss-oss-api",
  "exp": 1700000000,
  "iat": 1699996400
}
```

### Route Permission Matrix

| Route Pattern | GET | POST/PATCH | Notes |
|---|---|---|---|
| `/api/v1/subscribers` | noc, billing, csr, tech | billing_admin | Create requires billing_admin |
| `/api/v1/wallets/*` | billing_admin, csr | billing_admin | |
| `/api/v1/sessions/*` | noc, csr, tech | noc_engineer | CoA/PoD requires noc_engineer |
| `/api/v1/invoices/*` | billing_admin, csr | billing_admin | PDF download: all roles |
| `/api/v1/tickets/*` | all roles | csr, tech | |
| `/api/v1/lea/*` | noc + lea_flag | — | LEA flag is a separate JWT claim |
| `/api/v1/cgnat/*` | noc_engineer | — | |

### Audit Logging for Admin Actions

All POST, PATCH, DELETE requests by authenticated users must emit a structured audit log entry:

```json
{
  "timestamp": "2025-01-01T12:00:00Z",
  "level": "audit",
  "service": "api",
  "correlation_id": "abc-123",
  "actor_id": "admin_user_42",
  "actor_role": "billing_admin",
  "action": "subscriber.plan_change",
  "target_id": "subscriber:99",
  "http_method": "PATCH",
  "http_path": "/api/v1/subscribers/99",
  "result": "ok"
}
```

---

## 9.5 Webhook Security (Razorpay / BBPS)

```
Razorpay → POST /webhooks/razorpay
  Header: X-Razorpay-Signature: <sha256_hex>

Validation:
  expected = HMAC-SHA256(raw_body, RAZORPAY_WEBHOOK_SECRET)
  if expected != header_value → 400 Bad Request; log; do not process
```

- The raw request body must be read before any JSON parsing. Parsing before signature check allows body manipulation attacks.
- Failed validations increment `webhook_hmac_failures_total` counter and log at `warn` level with truncated payload hash.
- Webhook delivery failures (Razorpay retry exhausted): logged with full payload to a `webhook_failures` table for manual reconciliation.

---

## 9.6 DPDP Act Compliance Summary

| Requirement | Implementation |
|---|---|
| No plaintext PII storage | AES-GCM-256 application-layer encryption |
| Right to erasure | Subscriber deletion anonymizes PII fields; session history retains non-PII fields per DoT retention mandate |
| Data minimization | PII fields optional at registration; only collected when KYC required |
| 90-day key rotation | Automated Asynq job; idempotent and resumable |
| Audit trail | `encryption_keys` table; LEA audit log; admin action audit log |
| PII in logs | CI pipeline PII scanner; structured logs never serialize raw PII models |

---

## 9.7 Network Security

- RADIUS ports (1812, 1813 UDP) exposed only to MikroTik NAS IP range via host firewall (`ufw` / `iptables`). Not publicly accessible.
- API port (8080) exposed only to the reverse proxy container. The reverse proxy handles TLS termination and forwards to the API service on the internal network.
- PostgreSQL port (5432) bound to the internal Docker network only; not exposed on the host.
- Redis ports (6379, 26379) bound to the internal Docker network only.
